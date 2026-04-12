package virtualtun

import (
	"context"
	"sync"

	"github.com/ZhWn/gvisor/pkg/tcpip"
	"github.com/ZhWn/gvisor/pkg/tcpip/header"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
)

// Endpoint implements stack.LinkEndpoint using a channel for outbound packets
// and a saved dispatcher for inbound packet delivery.
// This is modeled after gvisor's pkg/tcpip/link/channel endpoint.
type Endpoint struct {
	mu         sync.RWMutex
	dispatcher stack.NetworkDispatcher
	onClose    func()
	closed     bool
	mtu        uint32
	linkAddr   tcpip.LinkAddress
	outbound   chan *stack.PacketBuffer
}

// NewEndpoint creates a channel-based LinkEndpoint.
func NewEndpoint(size int, mtu uint32, linkAddr tcpip.LinkAddress) *Endpoint {
	e := &Endpoint{
		mtu:      mtu,
		linkAddr: linkAddr,
		outbound: make(chan *stack.PacketBuffer, size),
	}
	return e
}

// MTU implements stack.LinkEndpoint.
func (e *Endpoint) MTU() uint32 {
	return e.mtu
}

// SetMTU implements stack.LinkEndpoint.
func (e *Endpoint) SetMTU(mtu uint32) {
	e.mtu = mtu
}

// MaxHeaderLength implements stack.LinkEndpoint.
func (e *Endpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress implements stack.LinkEndpoint.
func (e *Endpoint) LinkAddress() tcpip.LinkAddress {
	return e.linkAddr
}

// SetLinkAddress implements stack.LinkEndpoint.
func (e *Endpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.linkAddr = addr
}

// Capabilities implements stack.LinkEndpoint.
func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}

// Attach implements stack.LinkEndpoint.
func (e *Endpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = dispatcher
}

// IsAttached implements stack.LinkEndpoint.
func (e *Endpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dispatcher != nil
}

// Wait implements stack.LinkEndpoint.
func (e *Endpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.
func (e *Endpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.
func (e *Endpoint) AddHeader(pkt *stack.PacketBuffer) {}

// ParseHeader implements stack.LinkEndpoint.
func (e *Endpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	return true
}

// Close implements stack.LinkEndpoint.
func (e *Endpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.outbound)
	if e.onClose != nil {
		e.onClose()
	}
}

// SetOnCloseAction implements stack.LinkEndpoint.
func (e *Endpoint) SetOnCloseAction(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onClose = fn
}

// WritePackets implements stack.LinkEndpoint.
func (e *Endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	e.mu.RLock()
	closed := e.closed
	e.mu.RUnlock()
	if closed {
		return 0, &tcpip.ErrClosedForSend{}
	}

	n := 0
	for _, pkt := range pkts.AsSlice() {
		cloned := pkt.Clone()
		select {
		case e.outbound <- cloned:
			n++
		default:
			cloned.DecRef()
		}
	}
	return n, nil
}

// Read blocks until a packet is available from the outbound channel.
// Returns nil if the endpoint is closed.
func (e *Endpoint) Read() *stack.PacketBuffer {
	pkt, ok := <-e.outbound
	if !ok {
		return nil
	}
	return pkt
}

// ReadContext reads a packet with a context timeout.
func (e *Endpoint) ReadContext(ctx context.Context) *stack.PacketBuffer {
	select {
	case pkt := <-e.outbound:
		return pkt
	case <-ctx.Done():
		return nil
	}
}

// TryRead non-blocking read. Returns nil if no packet available.
func (e *Endpoint) TryRead() *stack.PacketBuffer {
	select {
	case pkt := <-e.outbound:
		return pkt
	default:
		return nil
	}
}

// InjectInbound delivers a packet into the gvisor stack via the saved dispatcher.
func (e *Endpoint) InjectInbound(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	e.mu.RLock()
	d := e.dispatcher
	e.mu.RUnlock()
	if d != nil {
		d.DeliverNetworkPacket(protocol, pkt)
	}
}
