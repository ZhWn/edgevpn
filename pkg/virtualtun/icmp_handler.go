package virtualtun

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/ZhWn/gvisor/pkg/tcpip"
	"github.com/ZhWn/gvisor/pkg/tcpip/checksum"
	"github.com/ZhWn/gvisor/pkg/tcpip/header"
	"github.com/ZhWn/gvisor/pkg/tcpip/network/ipv4"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/raw"
	"github.com/ZhWn/gvisor/pkg/waiter"
)

// ICMPEchoHandler handles ICMP echo requests by responding with echo replies.
// This enables ping between no-tun clients without involving the local OS.
type ICMPEchoHandler struct {
	s      *stack.Stack
	mu     sync.Mutex
	closed bool
}

// NewICMPEchoHandler creates a new ICMPEchoHandler.
func NewICMPEchoHandler(s *stack.Stack) *ICMPEchoHandler {
	return &ICMPEchoHandler{
		s: s,
	}
}

// Start installs the ICMP echo handler using a raw endpoint.
// It intercepts ICMP echo requests and responds with echo replies.
func (h *ICMPEchoHandler) Start() error {
	wq := &waiter.Queue{}
	ep, err := raw.NewEndpoint(h.s, ipv4.ProtocolNumber, header.ICMPv4ProtocolNumber, wq)
	if err != nil {
		return fmt.Errorf("failed to create raw ICMP endpoint: %w", convError(err))
	}
	go h.handleICMPv4Loop(ep, wq)
	return nil
}

func (h *ICMPEchoHandler) handleICMPv4Loop(ep tcpip.Endpoint, wq *waiter.Queue) {
	defer ep.Close()

	entry, eventCh := waiter.NewChannelEntry(waiter.ReadableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)

	buf := make([]byte, 65536)

	for {
		res, err := ep.Read(bytes.NewBuffer(buf), tcpip.ReadOptions{})
		if err != nil {
			if _, closed := err.(*tcpip.ErrClosedForReceive); closed {
				return
			}
			// Wait for notification
			select {
			case <-eventCh:
				continue
			case <-h.ctxDone():
				return
			}
		}

		data := buf[:res.Count]

		// Raw IPv4 ICMP: the data includes the IP header
		ipv4Hdr := header.IPv4(data)
		if !ipv4Hdr.IsValid(len(data)) {
			continue
		}
		icmpData := ipv4Hdr.Payload()
		if len(icmpData) < header.ICMPv4MinimumSize {
			continue
		}

		icmpHdr := header.ICMPv4(icmpData)
		if icmpHdr.Type() == header.ICMPv4Echo {
			h.sendEchoReply(ep, ipv4Hdr, icmpData)
		}
	}
}

func (h *ICMPEchoHandler) ctxDone() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{})
	if h.closed {
		close(ch)
	}
	return ch
}

func (h *ICMPEchoHandler) sendEchoReply(ep tcpip.Endpoint, ipHdr header.IPv4, icmpData []byte) {
	// Build echo reply: change type from Echo(8) to EchoReply(0), recalculate checksum
	replyICMP := make([]byte, len(icmpData))
	copy(replyICMP, icmpData)
	replyHdr := header.ICMPv4(replyICMP)
	replyHdr.SetType(header.ICMPv4EchoReply)

	// Recalculate ICMP checksum
	replyHdr.SetChecksum(0)
	replyHdr.SetChecksum(^checksum.Checksum(replyICMP, 0))

	// Build IP header with swapped addresses
	ipHdrLen := int(ipHdr.HeaderLength())
	ipHdrBuf := make([]byte, ipHdrLen)
	copy(ipHdrBuf, []byte(ipHdr))
	replyIPHdr := header.IPv4(ipHdrBuf)
	replyIPHdr.SetSourceAddress(ipHdr.DestinationAddress())
	replyIPHdr.SetDestinationAddress(ipHdr.SourceAddress())
	replyIPHdr.SetTotalLength(uint16(ipHdrLen + len(replyICMP)))
	replyIPHdr.SetChecksum(0)
	replyIPHdr.SetChecksum(^checksum.Checksum(ipHdrBuf, 0))

	// Combine IP header + ICMP payload
	fullPacket := append(ipHdrBuf, replyICMP...)

	_, err := ep.Write(bytes.NewBuffer(fullPacket), tcpip.WriteOptions{})
	if err != nil {
		// Log silently - the reply may fail if routing is misconfigured
	}
}

// Close marks the handler as closed.
func (h *ICMPEchoHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	return nil
}
