package virtualtun

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/ZhWn/gvisor/pkg/tcpip"
	"github.com/ZhWn/gvisor/pkg/tcpip/header"
	"github.com/ZhWn/gvisor/pkg/tcpip/network/ipv4"
	"github.com/ZhWn/gvisor/pkg/tcpip/network/ipv6"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/icmp"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/tcp"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/udp"
)

// VirtualTun implements io.ReadWriteCloser using gvisor's user-space network stack.
type VirtualTun struct {
	s        *stack.Stack
	ep       *Endpoint
	nicID    tcpip.NICID
	addr     tcpip.Address
	sn       tcpip.Subnet
	mtu      uint32
	subnetIP tcpip.Address
	maskLen  int
	ctx      context.Context
	cancel   context.CancelFunc
}

// convError converts a gvisor tcpip.Error to a Go error
func convError(err tcpip.Error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("tcpip error: %s", err.String())
}

// New creates a new VirtualTun with a gvisor network stack.
func New(localIP string, subnetCIDR string, mtu int) (*VirtualTun, error) {
	linkAddr := tcpip.LinkAddress(syntheticSrcMAC)

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
		HandleLocal: true,
	})

	nicID := tcpip.NICID(1)

	ep := NewEndpoint(256, uint32(mtu), linkAddr)

	if err := s.CreateNIC(nicID, ep); err != nil {
		s.Close()
		return nil, convError(err)
	}

	// Parse localIP
	ipParts := strings.Split(localIP, "/")
	addrStr := ipParts[0]
	rawIP := net.ParseIP(addrStr)
	if rawIP == nil {
		s.Close()
		return nil, fmt.Errorf("invalid IP address: %s", addrStr)
	}
	ipAddr := tcpip.AddrFromSlice(rawIP.To4())

	// Determine prefix length
	prefixLen := 24
	if len(ipParts) > 1 {
		_, ipNet, err := net.ParseCIDR(localIP)
		if err == nil {
			prefixLen, _ = ipNet.Mask.Size()
		}
	}

	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   ipAddr,
			PrefixLen: prefixLen,
		},
	}, stack.AddressProperties{}); err != nil {
		s.Close()
		return nil, convError(err)
	}

	// Parse the subnet for routing
	_, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("invalid subnet CIDR %q: %w", subnetCIDR, err)
	}

	subnetAddr := tcpip.AddrFromSlice(ipNet.IP.To4())
	maskLen, _ := ipNet.Mask.Size()
	mask := tcpip.AddressMask{}
	if maskLen > 0 && maskLen <= 32 {
		maskBytes := make([]byte, 4)
		for i := 0; i < 4; i++ {
			bits := maskLen - i*8
			if bits >= 8 {
				maskBytes[i] = 0xFF
			} else if bits > 0 {
				maskBytes[i] = byte(0xFF << (8 - bits))
			}
		}
		mask = tcpip.MaskFromBytes(maskBytes)
	}
	sn, err := tcpip.NewSubnet(subnetAddr, mask)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("failed to create subnet: %w", err)
	}

	// Set up routing table: VPN subnet + default route through our NIC
	defaultSubnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{}), tcpip.MaskFromBytes([]byte{0x00, 0x00, 0x00, 0x00}))
	s.SetRouteTable([]tcpip.Route{
		{
			Destination: sn,
			NIC:         nicID,
		},
		{
			Destination: defaultSubnet,
			NIC:         nicID,
		},
	})

	s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, false)

	ctx, cancel := context.WithCancel(context.Background())

	vt := &VirtualTun{
		s:        s,
		ep:       ep,
		nicID:    nicID,
		addr:     ipAddr,
		sn:       sn,
		mtu:      uint32(mtu),
		subnetIP: subnetAddr,
		maskLen:  maskLen,
		ctx:      ctx,
		cancel:   cancel,
	}

	// Pre-populate neighbor cache
	go vt.populateNeighborCache()

	return vt, nil
}

// populateNeighborCache adds static neighbor entries to avoid ARP delays.
func (vt *VirtualTun) populateNeighborCache() {
	if vt.maskLen >= 16 {
		maxHosts := 1 << uint(32-vt.maskLen)
		if maxHosts > 254 {
			maxHosts = 254
		}
		networkIP := vt.subnetIP.As4()
		for i := 1; i <= maxHosts; i++ {
			peerIP := tcpip.AddrFrom4([4]byte{networkIP[0], networkIP[1], networkIP[2], byte(i)})
			if peerIP == vt.addr {
				continue
			}
			_ = vt.s.AddStaticNeighbor(vt.nicID, ipv4.ProtocolNumber, peerIP, tcpip.LinkAddress(syntheticSrcMAC))
		}
	}
}

// Read implements io.Reader. Returns raw IP packets (no ethernet header),
// matching the behavior of a real TUN device.
func (vt *VirtualTun) Read(b []byte) (int, error) {
	for {
		pkt := vt.ep.ReadContext(vt.ctx)
		if pkt == nil {
			return 0, fmt.Errorf("read cancelled or closed")
		}

		// Only pass IPv4 packets
		if pkt.NetworkProtocolNumber == header.IPv4ProtocolNumber {
			// Extract raw IP packet data (no ethernet header, matching TUN behavior)
			view := pkt.ToView()
			if view != nil {
				data := view.AsSlice()
				n := copy(b, data)
				pkt.DecRef()
				return n, nil
			}
		}

		// Drop non-IPv4 packets
		pkt.DecRef()
	}
}

// Write implements io.Writer. Receives raw IP packets (no ethernet header),
// matching the behavior of a real TUN device.
func (vt *VirtualTun) Write(b []byte) (int, error) {
	// Validate it looks like an IPv4 packet
	if len(b) < header.IPv4MinimumSize {
		return 0, fmt.Errorf("packet too short (%d bytes)", len(b))
	}
	ipv4Hdr := header.IPv4(b)
	if !ipv4Hdr.IsValid(len(b)) {
		return 0, fmt.Errorf("invalid IPv4 header")
	}

	srcIP := ipv4Hdr.SourceAddress()
	if srcIP != vt.addr {
		_ = vt.s.AddStaticNeighbor(vt.nicID, ipv4.ProtocolNumber, srcIP, tcpip.LinkAddress(syntheticSrcMAC))
	}

	pkt, _, err := makePacket(b, header.IPv4ProtocolNumber)
	if err != nil {
		return 0, err
	}

	vt.ep.InjectInbound(header.IPv4ProtocolNumber, pkt)
	return len(b), nil
}

// Close implements io.Closer.
func (vt *VirtualTun) Close() error {
	vt.cancel()
	vt.ep.Close()
	vt.s.RemoveNIC(vt.nicID)
	vt.s.Close()
	return nil
}

// Stack returns the underlying gvisor stack.
func (vt *VirtualTun) Stack() *stack.Stack {
	return vt.s
}

// LocalAddr returns the VPN virtual IP address.
func (vt *VirtualTun) LocalAddr() tcpip.Address {
	return vt.addr
}

// Subnet returns the VPN subnet for routing.
func (vt *VirtualTun) Subnet() tcpip.Subnet {
	return vt.sn
}

// NICID returns the gvisor NIC identifier.
func (vt *VirtualTun) NICID() tcpip.NICID {
	return vt.nicID
}
