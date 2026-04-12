package virtualtun

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/ZhWn/gvisor/pkg/tcpip"
	"github.com/ZhWn/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/ZhWn/gvisor/pkg/tcpip/network/ipv4"
	"github.com/ZhWn/gvisor/pkg/tcpip/stack"
	"github.com/ZhWn/gvisor/pkg/tcpip/transport/tcp"
	"github.com/ZhWn/gvisor/pkg/waiter"

	"github.com/mudler/edgevpn/pkg/blockchain"
	"github.com/mudler/edgevpn/pkg/protocol"
	"github.com/mudler/edgevpn/pkg/types"
)

// SOCKS5Proxy is a SOCKS5 proxy that routes traffic through the gvisor stack.
type SOCKS5Proxy struct {
	listenAddr string
	s          *stack.Stack
	ledger     *blockchain.Ledger
	ln         net.Listener
	mu         sync.Mutex
	closed     bool
}

// NewSOCKS5Server creates a new SOCKS5 proxy server.
func NewSOCKS5Server(listenAddr string, s *stack.Stack, ledger *blockchain.Ledger) *SOCKS5Proxy {
	return &SOCKS5Proxy{
		listenAddr: listenAddr,
		s:          s,
		ledger:     ledger,
	}
}

// Start begins listening for SOCKS5 connections.
func (p *SOCKS5Proxy) Start() error {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", p.listenAddr, err)
	}
	p.ln = ln

	go p.acceptLoop()
	return nil
}

// Stop stops the SOCKS5 proxy server.
func (p *SOCKS5Proxy) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.ln != nil {
		return p.ln.Close()
	}
	return nil
}

func (p *SOCKS5Proxy) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handleConn(conn)
	}
}

func (p *SOCKS5Proxy) handleConn(conn net.Conn) {
	defer conn.Close()

	if err := p.readMethods(conn); err != nil {
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	req, err := p.readRequest(conn)
	if err != nil {
		return
	}

	switch req.cmd {
	case 0x01: // CONNECT
		p.handleConnect(conn, req)
	case 0x03: // UDP ASSOCIATE
		p.handleUDPAssociate(conn)
	default:
		p.sendReply(conn, 0x07)
	}
}

func (p *SOCKS5Proxy) readMethods(conn net.Conn) error {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != 0x05 {
		return fmt.Errorf("unsupported socks version: %d", hdr[0])
	}
	nMethods := int(hdr[1])
	buf := make([]byte, nMethods)
	_, err := io.ReadFull(conn, buf)
	return err
}

type socksRequest struct {
	cmd  byte
	addr string
	port uint16
}

func (p *SOCKS5Proxy) readRequest(conn net.Conn) (*socksRequest, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 0x05 {
		return nil, fmt.Errorf("unsupported socks version: %d", hdr[0])
	}

	cmd := hdr[1]
	atyp := hdr[3]

	var addr string
	var port uint16

	switch atyp {
	case 0x01: // IPv4
		var ip4 [4]byte
		if _, err := io.ReadFull(conn, ip4[:]); err != nil {
			return nil, err
		}
		addr = net.IP(ip4[:]).String()
	case 0x03: // Domain name
		var dl [1]byte
		if _, err := io.ReadFull(conn, dl[:]); err != nil {
			return nil, err
		}
		dom := make([]byte, dl[0])
		if _, err := io.ReadFull(conn, dom); err != nil {
			return nil, err
		}
		addr = string(dom)
	case 0x04: // IPv6
		var ip6 [16]byte
		if _, err := io.ReadFull(conn, ip6[:]); err != nil {
			return nil, err
		}
		addr = net.IP(ip6[:]).String()
	default:
		return nil, fmt.Errorf("unsupported address type: %d", atyp)
	}

	var portBytes [2]byte
	if _, err := io.ReadFull(conn, portBytes[:]); err != nil {
		return nil, err
	}
	port = binary.BigEndian.Uint16(portBytes[:])

	return &socksRequest{cmd: cmd, addr: addr, port: port}, nil
}

func (p *SOCKS5Proxy) sendReply(conn net.Conn, reply byte) {
	resp := []byte{0x05, reply, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	conn.Write(resp)
}

func (p *SOCKS5Proxy) handleConnect(conn net.Conn, req *socksRequest) {
	dstIP := p.resolveAddr(req.addr)
	if dstIP == nil {
		fmt.Printf("[SOCKS5] resolveAddr failed for %s\n", req.addr)
		p.sendReply(conn, 0x04)
		return
	}

	fmt.Printf("[SOCKS5] connecting to %s:%d (resolved: %s)\n", req.addr, req.port, dstIP)

	gAddr := tcpip.FullAddress{
		Addr: tcpip.AddrFromSlice(dstIP),
		Port: req.port,
	}

	tcpConn, err := gonet.DialTCP(p.s, gAddr, ipv4.ProtocolNumber)
	if err != nil {
		fmt.Printf("[SOCKS5] DialTCP failed: %v\n", err)
		p.sendReply(conn, 0x05)
		return
	}

	fmt.Printf("[SOCKS5] connection established to %s:%d\n", dstIP, req.port)
	p.sendReply(conn, 0x00)
	p.relay(conn, tcpConn)
}

func (p *SOCKS5Proxy) handleUDPAssociate(conn net.Conn) {
	resp := []byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	conn.Write(resp)

	buf := make([]byte, 1)
	for {
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
	}
}

func (p *SOCKS5Proxy) resolveAddr(addr string) net.IP {
	if p.ledger != nil {
		if ip := p.lookupLedger(addr); ip != nil {
			return ip
		}
	}

	if ip := net.ParseIP(addr); ip != nil {
		return ip.To4()
	}

	if ips, err := net.LookupIP(addr); err == nil && len(ips) > 0 {
		for _, ip := range ips {
			if ip.To4() != nil {
				return ip.To4()
			}
		}
	}

	return nil
}

func (p *SOCKS5Proxy) lookupLedger(name string) net.IP {
	if p.ledger == nil {
		return nil
	}

	if ip := net.ParseIP(name); ip != nil {
		ipStr := ip.String()
		value, found := p.ledger.GetKey(protocol.MachinesLedgerKey, ipStr)
		if found {
			machine := &types.Machine{}
			if err := value.Unmarshal(machine); err == nil {
				return net.ParseIP(machine.Address).To4()
			}
		}
	}

	data := p.ledger.CurrentData()
	if machines, ok := data[protocol.MachinesLedgerKey]; ok {
		for _, v := range machines {
			machine := &types.Machine{}
			if err := v.Unmarshal(machine); err == nil {
				if machine.Hostname == name {
					return net.ParseIP(machine.Address).To4()
				}
			}
		}
	}

	return nil
}

func (p *SOCKS5Proxy) relay(src, dst io.ReadWriteCloser) {
	defer src.Close()
	defer dst.Close()

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

// SetupInboundListener creates a TCP endpoint on the gvisor stack to listen
// for inbound connections from VPN peers. This enables TUN -> no-TUN communication.
func (p *SOCKS5Proxy) SetupInboundListener(port uint16) (uint16, error) {
	wq := &waiter.Queue{}

	ep, err := p.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, wq)
	if err != nil {
		return 0, convError(err)
	}

	addr := tcpip.FullAddress{Port: port}
	if bindErr := ep.Bind(addr); bindErr != nil {
		ep.Close()
		return 0, convError(bindErr)
	}

	if listenErr := ep.Listen(128); listenErr != nil {
		ep.Close()
		return 0, convError(listenErr)
	}

	gotAddr, _ := ep.GetLocalAddress()

	go func() {
		defer ep.Close()
		for {
			newEp, _, nerr := ep.Accept(nil)
			if nerr != nil {
				return
			}

			localAddr, lerr := newEp.GetLocalAddress()
			if lerr != nil {
				newEp.Close()
				continue
			}

			go p.handleInboundConnection(newEp, localAddr.Port)
		}
	}()

	return gotAddr.Port, nil
}

func (p *SOCKS5Proxy) handleInboundConnection(ep tcpip.Endpoint, localPort uint16) {
	defer ep.Close()

	targetAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	localConn, dialErr := net.Dial("tcp", targetAddr)
	if dialErr != nil {
		return
	}
	defer localConn.Close()

	gonetConn := gonet.NewTCPConn(&waiter.Queue{}, ep)
	p.relay(gonetConn, localConn)
}
