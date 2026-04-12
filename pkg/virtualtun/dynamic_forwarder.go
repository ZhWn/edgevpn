package virtualtun

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/ZhWn/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/tcp"
	"github.com/ZhWn/gvisor/pkg/waiter"
)

// DynamicForwarder uses gvisor's tcp.Forwarder to automatically handle
// incoming TCP connections on any port and forward them to the local OS.
// This eliminates the need for manual --bridge-ports configuration.
type DynamicForwarder struct {
	s      *stack.Stack
	mu     sync.Mutex
	closed bool
}

// NewDynamicForwarder creates a new DynamicForwarder.
func NewDynamicForwarder(s *stack.Stack) *DynamicForwarder {
	return &DynamicForwarder{
		s: s,
	}
}

// Start installs the forwarder as the TCP transport protocol handler.
// All incoming TCP SYN packets will be handled by the forwarder callback,
// which attempts to connect to 127.0.0.1:port on the local OS.
// If nothing is listening locally, the connection is RSTed.
func (f *DynamicForwarder) Start() error {
	fwd := tcp.NewForwarder(f.s, 0 /* default rcvWnd */, 100, f.handleRequest)
	f.s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	return nil
}

func (f *DynamicForwarder) handleRequest(r *tcp.ForwarderRequest) {
	dstPort := r.ID().LocalPort

	// Try to dial the local OS first to see if anything is listening
	targetAddr := fmt.Sprintf("127.0.0.1:%d", dstPort)
	localConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		// Nothing listening locally, send RST
		r.Complete(true)
		return
	}
	defer localConn.Close()

	// Create the gvisor endpoint to complete the TCP handshake
	wq := &waiter.Queue{}
	ep, epErr := r.CreateEndpoint(wq)
	if epErr != nil {
		r.Complete(true)
		localConn.Close()
		return
	}
	r.Complete(false)

	gonetConn := gonet.NewTCPConn(wq, ep)
	f.relay(gonetConn, localConn)
}

func (f *DynamicForwarder) relay(src, dst io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(dst, src)
	}()

	go func() {
		defer wg.Done()
		io.Copy(src, dst)
	}()

	wg.Wait()
}

// Close marks the forwarder as closed.
func (f *DynamicForwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return nil
}
