//
// Package chord is used to provide an implementation of the Chord network protocol.
//
package chord

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"hash"
	"time"
)

// Transport implements the methods needed for a Chord ring
type Transport interface {
	// Gets a list of the vnodes on the box
	ListVnodes(string) ([]*Vnode, error)

	// Ping a Vnode, check for liveness
	Ping(*Vnode) (bool, error)

	// Request a nodes predecessor
	GetPredecessor(*Vnode) (*Vnode, error)

	// Notify our successor of ourselves
	Notify(target, self *Vnode) ([]*Vnode, error)

	// Find a successor
	FindSuccessors(*Vnode, int, []byte) ([]*Vnode, error)

	// Clears a predecessor if it matches a given vnode. Used to leave.
	ClearPredecessor(target, self *Vnode) error

	// Instructs a node to skip a given successor. Used to leave.
	SkipSuccessor(target, self *Vnode) error

	// Register for an RPC callbacks
	Register(*Vnode, VnodeRPC)
}

// VnodeRPC contains methods to invoke on the registered vnodes
type VnodeRPC interface {
	GetPredecessor() (*Vnode, error)
	Notify(*Vnode) ([]*Vnode, error)
	FindSuccessors(int, []byte) ([]*Vnode, error)
	ClearPredecessor(*Vnode) error
	SkipSuccessor(*Vnode) error
}

// Delegate to notify on ring events
type Delegate interface {
	NewPredecessor(local, remoteNew, remotePrev *Vnode)
	Leaving(local, pred, succ *Vnode)
	PredecessorLeaving(local, remote *Vnode)
	SuccessorLeaving(local, remote *Vnode)
	Shutdown()
}

// Meta holds metadata for a node
type Meta map[string][]byte

// MarshalBinary marshals Meta to bytes
func (meta Meta) MarshalBinary() ([]byte, error) {
	lines := make([][]byte, len(meta))

	i := 0
	for k, v := range meta {
		lines[i] = append(append([]byte(k), []byte("=")...), v...)
		i++
	}

	return bytes.Join(lines, []byte(" ")), nil
}

// UnmarshalBinary unmarshals bytes into Meta
func (meta Meta) UnmarshalBinary(b []byte) error {
	lines := bytes.Split(b, []byte(" "))
	for _, line := range lines {
		arr := bytes.Split(line, []byte("="))
		if len(arr) != 2 {
			return fmt.Errorf("invalid data: %s", line)
		}
		meta[string(arr[0])] = arr[1]
	}
	return nil
}

// Config for Chord nodes
type Config struct {
	Hostname      string           // Local host name
	Meta          Meta             // User defined metadata
	NumVnodes     int              // Number of vnodes per physical node
	HashFunc      func() hash.Hash `json:"-"` // Hash function to use
	StabilizeMin  time.Duration    // Minimum stabilization time
	StabilizeMax  time.Duration    // Maximum stabilization time
	NumSuccessors int              // Number of successors to maintain
	Delegate      Delegate         `json:"-"` // Invoked to handle ring events
	hashBits      int              // Bit size of the hash function
}

// Represents a local Vnode
type localVnode struct {
	Vnode
	ring        *Ring
	successors  []*Vnode
	finger      []*Vnode
	lastFinger  int
	predecessor *Vnode
	stabilized  time.Time
	timer       *time.Timer
}

// Ring stores the state required for a Chord ring
type Ring struct {
	config     *Config
	transport  Transport
	vnodes     []*localVnode
	delegateCh chan func()
	shutdown   chan bool
}

// DefaultConfig returns the default Ring configuration
func DefaultConfig(hostname string) *Config {
	return &Config{
		Hostname:      hostname,
		Meta:          make(Meta),
		NumVnodes:     8,
		HashFunc:      sha1.New, // sha1
		StabilizeMin:  time.Duration(15 * time.Second),
		StabilizeMax:  time.Duration(45 * time.Second),
		NumSuccessors: 8,
		Delegate:      nil,
		hashBits:      160, // 160bit hash function for sha1
	}
}

// Create a new Chord ring given the config and transport
func Create(conf *Config, trans Transport) (*Ring, error) {
	// Initialize the hash bits
	conf.hashBits = conf.HashFunc().Size() * 8

	// Create and initialize a ring
	ring := &Ring{}
	ring.init(conf, trans)
	ring.setLocalSuccessors()
	ring.schedule()

	return ring, nil
}

// Join an existing Chord ring
func Join(conf *Config, trans Transport, existing string) (*Ring, error) {
	// Initialize the hash bits
	conf.hashBits = conf.HashFunc().Size() * 8

	// Request a list of Vnodes from the remote host
	hosts, err := trans.ListVnodes(existing)
	if err != nil {
		return nil, err
	}
	if hosts == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("remote host has no vnodes")
	}

	// Create a ring
	ring := &Ring{}
	ring.init(conf, trans)

	// Acquire a live successor for each Vnode
	for _, vn := range ring.vnodes {
		// Get the nearest remote vnode
		nearest := nearestVnodeToKey(hosts, vn.Id)

		// Query for a list of successors to this Vnode
		succs, err := trans.FindSuccessors(nearest, conf.NumSuccessors, vn.Id)
		if err != nil {
			//return nil, fmt.Errorf("Failed to find successor for vnodes! Got %s", err)
			return nil, err
		}
		if succs == nil || len(succs) == 0 {
			return nil, fmt.Errorf("successor vnodes not found")
		}

		// Assign the successors
		for idx, s := range succs {
			vn.successors[idx] = s
		}
	}

	// Start delegate handler
	if ring.config.Delegate != nil {
		go ring.delegateHandler()
	}
	// Do a fast stabilization, will schedule regular execution
	for _, vn := range ring.vnodes {
		vn.stabilize()
	}

	//ring.schedule()

	return ring, nil
}

// Leave a given Chord ring and shuts down the local vnodes
func (r *Ring) Leave() error {
	// Shutdown the vnodes first to avoid further stabilization runs
	r.stopVnodes()

	// Instruct each vnode to leave
	var err error
	for _, vn := range r.vnodes {
		err = mergeErrors(err, vn.leave())
	}

	// Wait for the delegate callbacks to complete
	r.stopDelegate()
	return err
}

// Shutdown shuts down the local processes in a given Chord ring
// Blocks until all the vnodes terminate.
func (r *Ring) Shutdown() {
	r.stopVnodes()
	r.stopDelegate()
}

// LookupHash does a lookup for up to N successors of a hash.  It returns the predecessor and up
// to N successors. The hash size must match the hash function used when init'ing the ring.
func (r *Ring) LookupHash(n int, hash []byte) (*Vnode, []*Vnode, error) {
	// Ensure that n is sane
	if n > r.config.NumSuccessors {
		return nil, nil, fmt.Errorf("cannot ask for more successors than NumSuccessors")
	}

	// Find the nearest local vnode
	nearest := r.nearestVnode(hash)
	pred := nearest.Vnode
	// Use the nearest node for the lookup
	successors, err := nearest.FindSuccessors(n, hash)
	if err != nil {
		return &pred, nil, err
	}

	// Trim the nil successors
	for successors[len(successors)-1] == nil {
		successors = successors[:len(successors)-1]
	}
	return &pred, successors, nil
}

// Lookup does a lookup for up to N successors on the hash of a key.  It returns the hash of the key used to
// perform the lookup, the closest vnode and up to N successors.
func (r *Ring) Lookup(n int, key []byte) ([]byte, *Vnode, []*Vnode, error) {
	// Hash the key
	h := r.config.HashFunc()
	h.Write(key)
	kh := h.Sum(nil)

	nearest, succs, err := r.LookupHash(n, kh)
	return kh, nearest, succs, err
}
