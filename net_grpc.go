package chord

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	context "golang.org/x/net/context"
	"google.golang.org/grpc"
)

var (
	errTimedOut = errors.New("timed out")
)

type rpcOutConn struct {
	host   string
	conn   *grpc.ClientConn
	client ChordClient
	used   time.Time
}

// GRPCTransport used by chord
type GRPCTransport struct {
	server   *grpc.Server
	lock     sync.RWMutex
	local    map[string]*localRPC
	poolLock sync.Mutex
	pool     map[string][]*rpcOutConn
	shutdown int32
	timeout  time.Duration
	maxIdle  time.Duration
}

// NewGRPCTransport creates a new grpc transport using the provided listener
// and grpc server.
func NewGRPCTransport(gserver *grpc.Server, rpcTimeout, connMaxIdle time.Duration) *GRPCTransport {
	gt := &GRPCTransport{
		server:  gserver,
		local:   map[string]*localRPC{},
		pool:    map[string][]*rpcOutConn{},
		timeout: rpcTimeout,
		maxIdle: connMaxIdle,
	}

	RegisterChordServer(gt.server, gt)

	go gt.reapOld()

	return gt
}

// Closes old outbound connections
func (cs *GRPCTransport) reapOld() {
	for {
		if atomic.LoadInt32(&cs.shutdown) == 1 {
			return
		}
		time.Sleep(30 * time.Second)
		cs.reapOnce()
	}
}

func (cs *GRPCTransport) reapOnce() {
	cs.poolLock.Lock()
	defer cs.poolLock.Unlock()
	for host, conns := range cs.pool {
		max := len(conns)
		for i := 0; i < max; i++ {
			if time.Since(conns[i].used) > cs.maxIdle {
				conns[i].conn.Close()
				conns[i], conns[max-1] = conns[max-1], nil
				max--
				i--
			}
		}
		// Trim any idle conns
		cs.pool[host] = conns[:max]
	}
}

// Register vnode rpc's for a vnode.
func (cs *GRPCTransport) Register(v *Vnode, o VnodeRPC) {
	key := v.StringID()
	cs.lock.Lock()
	cs.local[key] = &localRPC{v, o}
	cs.lock.Unlock()
}

// ListVnodes gets a list of the vnodes on the box
func (cs *GRPCTransport) ListVnodes(host string) ([]*Vnode, error) {
	// Get a conn
	out, err := cs.getConn(host)
	if err != nil {
		return nil, err
	}

	// Response channels
	respChan := make(chan []*Vnode, 1)
	errChan := make(chan error, 1)

	go func() {
		le, err := out.client.ListVnodesServe(context.Background(), &StringParam{Value: host})
		// Return the connection
		cs.returnConn(out)

		if err == nil {
			respChan <- le.Vnodes
		} else {
			errChan <- err
		}

	}()

	select {
	case <-time.After(cs.timeout):
		return nil, errTimedOut
	case err := <-errChan:
		return nil, err
	case res := <-respChan:
		return res, nil
	}
}

// Ping a Vnode, check for liveness
func (cs *GRPCTransport) Ping(target *Vnode) (bool, error) {
	out, err := cs.getConn(target.Host)
	if err != nil {
		return false, err
	}

	// Response channels
	respChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	go func() {

		be, err := out.client.PingServe(context.Background(), target)
		// Return the connection
		cs.returnConn(out)

		if err == nil {
			respChan <- be.Ok
		} else {
			errChan <- err
		}

	}()

	select {
	case <-time.After(cs.timeout):
		return false, errTimedOut
	case err := <-errChan:
		return false, err
	case res := <-respChan:
		return res, nil
	}
}

// GetPredecessor requests a vnode's predecessor
func (cs *GRPCTransport) GetPredecessor(vn *Vnode) (*Vnode, error) {
	// Get a conn
	out, err := cs.getConn(vn.Host)
	if err != nil {
		return nil, err
	}

	respChan := make(chan *Vnode, 1)
	errChan := make(chan error, 1)

	go func(vnode *Vnode) {
		//
		vnd, err := out.client.GetPredecessorServe(context.Background(), vnode)
		// Return the connection
		cs.returnConn(out)

		if err == nil {
			respChan <- vnd
		} else {
			errChan <- err
		}

	}(vn)

	select {
	case <-time.After(cs.timeout):
		return nil, errTimedOut
	case err := <-errChan:
		return nil, err
	case res := <-respChan:
		return res, nil
	}
}

// Notify our successor of ourselves
func (cs *GRPCTransport) Notify(target, self *Vnode) ([]*Vnode, error) {
	// Get a conn
	out, err := cs.getConn(target.Host)
	if err != nil {
		return nil, err
	}

	respChan := make(chan []*Vnode, 1)
	errChan := make(chan error, 1)

	go func() {
		le, err := out.client.NotifyServe(context.Background(), &VnodePair{Target: target, Self: self})
		cs.returnConn(out)

		if err == nil {
			respChan <- le.Vnodes
		} else {
			errChan <- err
		}

	}()

	select {
	case <-time.After(cs.timeout):
		return nil, errTimedOut
	case err := <-errChan:
		return nil, err
	case res := <-respChan:
		return res, nil
	}
}

// FindSuccessors given the vnode upto n successors
func (cs *GRPCTransport) FindSuccessors(vn *Vnode, n int, k []byte) ([]*Vnode, error) {
	// Get a conn
	out, err := cs.getConn(vn.Host)
	if err != nil {
		return nil, err
	}

	respChan := make(chan []*Vnode, 1)
	errChan := make(chan error, 1)

	go func() {
		req := &FindSuccReq{VN: vn, Count: int32(n), Key: k}
		le, err := out.client.FindSuccessorsServe(context.Background(), req)
		// Return the connection
		cs.returnConn(out)

		if err == nil {
			respChan <- le.Vnodes
		} else {
			errChan <- err
		}

	}()

	select {
	case <-time.After(cs.timeout):
		return nil, errTimedOut
	case err := <-errChan:
		return nil, err
	case res := <-respChan:
		return res, nil
	}
}

// ClearPredecessor clears a predecessor if it matches a given vnode. Used to leave.
func (cs *GRPCTransport) ClearPredecessor(target, self *Vnode) error {
	// Get a conn
	out, err := cs.getConn(target.Host)
	if err != nil {
		return err
	}

	respChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	go func() {
		_, err := out.client.ClearPredecessorServe(context.Background(), &VnodePair{Target: target, Self: self})
		// Return the connection
		cs.returnConn(out)
		if err == nil {
			respChan <- true
		} else {
			errChan <- err
		}

	}()

	select {
	case <-time.After(cs.timeout):
		return errTimedOut
	case err := <-errChan:
		return err
	case <-respChan:
		return nil
	}
}

// SkipSuccessor instructs a node to skip a given successor. Used to leave.
func (cs *GRPCTransport) SkipSuccessor(target, self *Vnode) error {

	// Get a conn
	out, err := cs.getConn(target.Host)
	if err != nil {
		return err
	}

	respChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	go func() {
		_, err := out.client.SkipSuccessorServe(context.Background(), &VnodePair{Target: target, Self: self})
		// Return the connection
		cs.returnConn(out)

		if err == nil {
			respChan <- true
		} else {
			errChan <- err
		}

	}()

	select {
	case <-time.After(cs.timeout):
		return errTimedOut
	case err := <-errChan:
		return err
	case <-respChan:
		return nil
	}
}

// Gets an outbound connection to a host
func (cs *GRPCTransport) getConn(host string) (*rpcOutConn, error) {
	// Check if we have a conn cached
	var out *rpcOutConn
	cs.poolLock.Lock()
	if atomic.LoadInt32(&cs.shutdown) == 1 {
		cs.poolLock.Unlock()
		return nil, fmt.Errorf("TCP transport is shutdown")
	}
	list, ok := cs.pool[host]
	if ok && len(list) > 0 {
		out = list[len(list)-1]
		list = list[:len(list)-1]
		cs.pool[host] = list
	}
	cs.poolLock.Unlock()
	// Make a new connection
	if out == nil {
		conn, err := grpc.Dial(host, grpc.WithInsecure())
		if err == nil {
			return &rpcOutConn{
				host:   host,
				client: NewChordClient(conn),
				conn:   conn,
				used:   time.Now(),
			}, nil
		}
		return nil, err
	}
	// return an existing connection
	return out, nil
}

func (cs *GRPCTransport) returnConn(o *rpcOutConn) {
	// Update the last used time
	o.used = time.Now()
	// Push back into the pool
	cs.poolLock.Lock()
	defer cs.poolLock.Unlock()
	if atomic.LoadInt32(&cs.shutdown) == 1 {
		o.conn.Close()
		return
	}
	list, _ := cs.pool[o.host]
	cs.pool[o.host] = append(list, o)
}

// Checks for a local vnode
func (cs *GRPCTransport) get(vn *Vnode) (VnodeRPC, bool) {
	key := vn.StringID()

	cs.lock.RLock()
	defer cs.lock.RUnlock()

	w, ok := cs.local[key]
	if ok {
		return w.obj, ok
	}
	return nil, ok
}

// ListVnodesServe is the server side call
func (cs *GRPCTransport) ListVnodesServe(ctx context.Context, in *StringParam) (*VnodeList, error) {
	// Generate all the local clients
	vnodes := make([]*Vnode, 0, len(cs.local))
	// Build list
	cs.lock.RLock()
	for _, v := range cs.local {
		vnodes = append(vnodes, v.vnode)
	}
	cs.lock.RUnlock()

	return &VnodeList{Vnodes: vnodes}, nil
}

// PingServe serves a ping request
func (cs *GRPCTransport) PingServe(ctx context.Context, in *Vnode) (*Bool, error) {
	_, ok := cs.get(in)
	if ok {
		return &Bool{Ok: ok}, nil
	}
	return &Bool{}, fmt.Errorf("target vnode not found: %s/%x", in.Host, in.Id)
}

// NotifyServe serves a notify request
func (cs *GRPCTransport) NotifyServe(ctx context.Context, in *VnodePair) (*VnodeList, error) {
	var (
		obj, ok = cs.get(in.Target)
		resp    = &VnodeList{}
		err     error
	)

	if ok {
		var nodes []*Vnode
		if nodes, err = obj.Notify(in.Self); err == nil {
			resp.Vnodes = trimSlice(nodes)
		}
	} else {
		err = fmt.Errorf("target vnode not found: %s/%x", in.Target.Host, in.Target.Id)
	}

	return resp, err
}

// GetPredecessorServe serves a GetPredecessor request
func (cs *GRPCTransport) GetPredecessorServe(ctx context.Context, in *Vnode) (*Vnode, error) {
	obj, ok := cs.get(in)

	var (
		vn  *Vnode
		err error
	)
	if ok {
		vn, err = obj.GetPredecessor()
		if err == nil {

			//
			// TODO: Revisit WHY is it returning nil? (I.E. PREDECESSOR IS NIL)
			//

			if vn == nil {
				vn = &Vnode{}
			}
		}
	} else {
		err = fmt.Errorf("target vnode not found: %s/%x", in.Host, in.Id)
	}

	return vn, err
}

// FindSuccessorsServe serves a FindSuccessors request
func (cs *GRPCTransport) FindSuccessorsServe(ctx context.Context, in *FindSuccReq) (*VnodeList, error) {
	var (
		obj, ok = cs.get(in.VN)
		resp    = &VnodeList{}
		err     error
	)

	if ok {
		var nodes []*Vnode
		if nodes, err = obj.FindSuccessors(int(in.Count), in.Key); err == nil {
			resp.Vnodes = trimSlice(nodes)
		}
	} else {
		err = fmt.Errorf("target vnode not found: %s/%x", in.VN.Host, in.VN.Id)
	}

	return resp, err
}

// ClearPredecessorServe serves a ClearPredecessor request
func (cs *GRPCTransport) ClearPredecessorServe(ctx context.Context, in *VnodePair) (*Response, error) {
	var (
		obj, ok = cs.get(in.Target)
		resp    = &Response{}
		err     error
	)

	if ok {
		err = obj.ClearPredecessor(in.Self)
	} else {
		err = fmt.Errorf("target vnode not found: %s/%x", in.Target.Host, in.Target.Id)
	}

	return resp, err
}

// SkipSuccessorServe serves a SkipSuccessor request
func (cs *GRPCTransport) SkipSuccessorServe(ctx context.Context, in *VnodePair) (*Response, error) {
	var (
		obj, ok = cs.get(in.Target)
		resp    = &Response{}
		err     error
	)

	if ok {
		err = obj.SkipSuccessor(in.Self)
	} else {
		err = fmt.Errorf("target vnode not found: %s/%x", in.Target.Host, in.Target.Id)
	}

	return resp, err
}

// Shutdown the TCP transport
func (cs *GRPCTransport) Shutdown() {
	atomic.StoreInt32(&cs.shutdown, 1)

	//
	// TODO: remove this logic.  This should be handled by the entity that instantiated the grpc
	// instance.
	//

	// Drain and stop grpc server
	cs.server.GracefulStop()
	// Close all the outbound
	cs.poolLock.Lock()
	for _, conns := range cs.pool {
		for _, out := range conns {
			out.conn.Close()
		}
	}
	cs.pool = nil
	cs.poolLock.Unlock()
}
