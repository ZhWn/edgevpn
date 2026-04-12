package virtualtun

import (
	"encoding/binary"

	"github.com/ZhWn/gvisor/pkg/buffer"
	"github.com/ZhWn/gvisor/pkg/tcpip"
	"github.com/ZhWn/gvisor/pkg/tcpip/header"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
	"github.com/songgao/packets/ethernet"
)

const (
	// EtherLen is the size of our synthetic ethernet header
	// 6 bytes dst MAC + 6 bytes src MAC + 2 bytes ethertype = 14 bytes
	EtherLen = 14

	// EtherTypeIPv4 is the ethertype for IPv4
	EtherTypeIPv4 = 0x0800

	// EtherTypeIPv6 is the ethertype for IPv6
	EtherTypeIPv6 = 0x86DD

	// syntheticSrcMAC is the synthetic source MAC address
	syntheticSrcMAC = "\x02\x00\x00\x00\x00\x01"

	// syntheticDstMAC is the synthetic destination MAC address
	syntheticDstMAC = "\x02\x00\x00\x00\x00\x02"
)

// packetToFrame wraps a PacketBuffer's IP payload in a synthetic ethernet frame.
// The resulting frame can be fed into the existing EdgeVPN packet pipeline.
func packetToFrame(pkt *stack.PacketBuffer, mtu int) ethernet.Frame {
	// Use ToView to get the full packet data as a contiguous view
	view := pkt.ToView()
	if view == nil {
		var frame ethernet.Frame
		frame.Resize(EtherLen)
		return frame
	}

	data := view.AsSlice()
	if len(data) == 0 {
		var frame ethernet.Frame
		frame.Resize(EtherLen)
		return frame
	}

	// Determine ethertype from packet's network protocol number
	var ethertype uint16
	switch pkt.NetworkProtocolNumber {
	case header.IPv4ProtocolNumber:
		ethertype = EtherTypeIPv4
	case header.IPv6ProtocolNumber:
		ethertype = EtherTypeIPv6
	default:
		ethertype = EtherTypeIPv4
	}

	// Build ethernet frame: [dstMAC 6][srcMAC 6][ethertype 2][payload]
	frameSize := EtherLen + len(data)
	if mtu > 0 && frameSize > mtu {
		frameSize = mtu
		if len(data) > frameSize-EtherLen {
			data = data[:frameSize-EtherLen]
		}
	}

	var frame ethernet.Frame
	frame.Resize(frameSize)

	// dst MAC
	copy(frame[0:6], syntheticDstMAC)
	// src MAC
	copy(frame[6:12], syntheticSrcMAC)
	// ethertype (big-endian)
	binary.BigEndian.PutUint16(frame[12:14], ethertype)
	// payload
	copy(frame[EtherLen:], data)

	return frame
}

// frameToPacket strips the synthetic ethernet header from raw bytes
// and creates a stack.PacketBuffer for injection into gvisor.
// Returns the packet, the detected network protocol, and any error.
func frameToPacket(data []byte) (*stack.PacketBuffer, tcpip.NetworkProtocolNumber, error) {
	if len(data) < EtherLen {
		// No ethernet header, treat as raw IP
		return makePacket(data, header.IPv4ProtocolNumber)
	}

	ethertype := binary.BigEndian.Uint16(data[12:14])
	payload := data[EtherLen:]

	var proto tcpip.NetworkProtocolNumber
	switch ethertype {
	case EtherTypeIPv4:
		proto = header.IPv4ProtocolNumber
	case EtherTypeIPv6:
		proto = header.IPv6ProtocolNumber
	default:
		// Default to IPv4
		proto = header.IPv4ProtocolNumber
	}

	return makePacket(payload, proto)
}

// makePacket creates a stack.PacketBuffer from raw IP payload data.
func makePacket(payload []byte, proto tcpip.NetworkProtocolNumber) (*stack.PacketBuffer, tcpip.NetworkProtocolNumber, error) {
	// Create a buffer from the payload
	buf := buffer.MakeWithData(payload)

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buf,
	})

	pkt.NetworkProtocolNumber = proto

	return pkt, proto, nil
}
