// Copyright © 2021-2022 Ettore Di Giacinto <mudler@mocaccino.org>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vpn

import (
	"fmt"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// NetStack 封装 gvisor 用户态网络协议栈
type NetStack struct {
	Stack   *stack.Stack
	LinkEP  *channel.Endpoint
	NICID   tcpip.NICID
	LocalIP tcpip.Address
	Subnet  tcpip.Subnet
}

// NewNetStack 创建用户态网络栈
func NewNetStack(ip string, mtu int) (*NetStack, error) {
	// 1. 解析 IP 地址和前缀长度
	ipNet, err := parseCIDR(ip)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR: %w", err)
	}

	localIP := tcpip.AddrFrom4([4]byte(ipNet.IP.To4()))
	prefixLen, _ := ipNet.Mask.Size()

	// 2. 创建虚拟网卡（内存通道）
	linkEP := channel.New(1024, uint32(mtu), "")

	// 3. 创建协议栈
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
		},
		HandleLocal: true, // 自动处理本地回环流量
	})

	// 4. 创建 NIC
	nicID := tcpip.NICID(1)
	if tcpErr := s.CreateNIC(nicID, linkEP); tcpErr != nil {
		return nil, fmt.Errorf("createNIC failed: %s", tcpErr)
	}

	// 5. 添加 IP 地址
	addr := tcpip.ProtocolAddress{
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   localIP,
			PrefixLen: prefixLen,
		},
	}
	if tcpErr := s.AddProtocolAddress(nicID, addr, stack.AddressProperties{}); tcpErr != nil {
		return nil, fmt.Errorf("add address failed: %s", tcpErr)
	}

	// 6. 设置路由表
	subnetPrefix, err := tcpip.NewSubnet(
		localIP,
		tcpip.MaskFrom(fmt.Sprintf("/%d", prefixLen)),
	)
	if err != nil {
		return nil, err
	}

	// 默认路由
	defaultSubnet, _ := tcpip.NewSubnet(
		tcpip.AddrFrom4([4]byte{0, 0, 0, 0}),
		tcpip.MaskFrom("/0"),
	)

	s.SetRouteTable([]tcpip.Route{
		{
			Destination: subnetPrefix,
			NIC:         nicID,
		},
		{
			Destination: defaultSubnet,
			NIC:         nicID,
		},
	})

	return &NetStack{
		Stack:   s,
		LinkEP:  linkEP,
		NICID:   nicID,
		LocalIP: localIP,
		Subnet:  subnetPrefix,
	}, nil
}

// Close 关闭协议栈
func (ns *NetStack) Close() {
	if ns.Stack != nil {
		ns.Stack.Close()
	}
}

// parseCIDR 解析 CIDR 地址
func parseCIDR(cidr string) (*net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	return ipNet, nil
}
