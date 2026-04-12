package virtualtun

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/ZhWn/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/udp"
	"github.com/ZhWn/gvisor/pkg/waiter"
)

// UDPDynamicForwarder uses gvisor's udp.Forwarder to automatically handle
// incoming UDP packets on any port and forward them to the local OS.
type UDPDynamicForwarder struct {
	s      *stack.Stack
	mu     sync.Mutex
	closed bool
}

// NewUDPDynamicForwarder creates a new UDPDynamicForwarder.
func NewUDPDynamicForwarder(s *stack.Stack) *UDPDynamicForwarder {
	return &UDPDynamicForwarder{
		s: s,
	}
}

// Start installs the forwarder as the UDP transport protocol handler.
// All incoming UDP packets will be handled by the forwarder callback,
// which attempts to connect to 127.0.0.1:port on the local OS.
// If nothing is listening locally, the stack sends ICMP port unreachable.
func (f *UDPDynamicForwarder) Start() error {
	fwd := udp.NewForwarder(f.s, f.handleRequest)
	f.s.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)
	return nil
}

func (f *UDPDynamicForwarder) handleRequest(req *udp.ForwarderRequest) bool {
	dstPort := req.ID().LocalPort

	// Try to dial the local OS to see if anything is listening
	targetAddr := fmt.Sprintf("127.0.0.1:%d", dstPort)
	localConn, err := net.Dial("udp", targetAddr)
	if err != nil {
		return false // let stack send ICMP port unreachable
	}

	wq := &waiter.Queue{}
	ep, epErr := req.CreateEndpoint(wq)
	if epErr != nil {
		localConn.Close()
		return false
	}

	gvisorConn := gonet.NewUDPConn(wq, ep)
	go f.relay(gvisorConn, localConn)
	return true
}

func (f *UDPDynamicForwarder) relay(gvisorConn, localConn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(localConn, gvisorConn)
	}()

	go func() {
		defer wg.Done()
		io.Copy(gvisorConn, localConn)
	}()

	wg.Wait()
}

// Close marks the forwarder as closed.
func (f *UDPDynamicForwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return nil
}
