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
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/mudler/edgevpn/pkg/blockchain"
	"github.com/mudler/edgevpn/pkg/node"
	"github.com/mudler/edgevpn/pkg/protocol"
	"github.com/mudler/edgevpn/pkg/types"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// InboundStreamHandler 创建处理来自 P2P 网络的入站 IP 包的流处理器
// 注意：必须与 TUN 模式兼容（直接接收原始 IP 包，无长度前缀）
func InboundStreamHandler(l *blockchain.Ledger, ns *NetStack, nc node.Config) func(network.Stream) {
	return func(stream network.Stream) {
		defer stream.Close()

		// 验证 Peer 身份
		if !verifyInboundPeer(stream, l, nc) {
			stream.Reset()
			return
		}

		// 直接将流中的数据注入到 netstack
		// 与 TUN 模式一样，使用 io.Copy 的方式
		// 但这里我们需要边读边注入，因为 netstack 需要 PacketBuffer
		buf := make([]byte, 1500) // MTU 大小
		for {
			n, err := stream.Read(buf)
			if err != nil {
				return
			}

			if n == 0 {
				continue
			}

			// 创建 PacketBuffer 并注入
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(buf[:n]),
			})
			ns.LinkEP.InjectInbound(ipv4.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}
}

// verifyInboundPeer 验证入站连接的 Peer 身份
func verifyInboundPeer(stream network.Stream, l *blockchain.Ledger, nc node.Config) bool {
	// 检查 PeerTable（静态配置）
	if len(nc.PeerTable) > 0 {
		found := false
		for _, p := range nc.PeerTable {
			if p.String() == stream.Conn().RemotePeer().String() {
				found = true
				break
			}
		}
		return found
	}

	// 检查区块链中的机器注册
	return l.Exists(protocol.MachinesLedgerKey,
		func(d blockchain.Data) bool {
			machine := &types.Machine{}
			d.Unmarshal(machine)
			return machine.PeerID == stream.Conn().RemotePeer().String()
		})
}
