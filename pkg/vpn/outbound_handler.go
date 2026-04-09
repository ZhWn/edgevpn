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
	"context"
	"fmt"

	"github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/mudler/edgevpn/pkg/blockchain"
	"github.com/mudler/edgevpn/pkg/protocol"
	"github.com/mudler/edgevpn/pkg/types"
)

// OutboundHandler 处理从 netstack 到 P2P 网络的出站流量
type OutboundHandler struct {
	Stack  *NetStack
	Ledger *blockchain.Ledger
	Host   host.Host
	Logger log.StandardLogger
}

// NewOutboundHandler 创建出站处理器
func NewOutboundHandler(stack *NetStack, ledger *blockchain.Ledger, host host.Host, logger log.StandardLogger) *OutboundHandler {
	return &OutboundHandler{
		Stack:  stack,
		Ledger: ledger,
		Host:   host,
		Logger: logger,
	}
}

// Start 启动出站流量处理循环
func (h *OutboundHandler) Start(ctx context.Context) error {
	h.Logger.Info("Starting outbound handler")

	for {
		select {
		case <-ctx.Done():
			h.Logger.Info("Outbound handler stopped")
			return nil
		default:
			// 从虚拟网卡读取 IP 包
			pkt := h.Stack.LinkEP.ReadContext(ctx)
			if pkt == nil {
				continue
			}

			// 获取网络层头部
			netHdr := pkt.NetworkHeader()
			if len(netHdr.Slice()) == 0 {
				h.Logger.Debug("No network header")
				pkt.DecRef()
				continue
			}

			// 获取整个包数据
			// 通过 AsSlices 获取所有数据片段
			slices := pkt.AsSlices()
			if len(slices) == 0 {
				h.Logger.Debug("Empty packet")
				pkt.DecRef()
				continue
			}

			// 合并所有片段
			var pktData []byte
			for _, s := range slices {
				pktData = append(pktData, s...)
			}

			if len(pktData) < 20 { // IPv4 最小头部
				h.Logger.Debug("Invalid IP packet size")
				pkt.DecRef()
				continue
			}

			// 解析目标 IP（IPv4）
			// IPv4 头部: 字节 16-19 是目标地址
			dstIP := pktData[16:20]
			dstIPStr := fmt.Sprintf("%d.%d.%d.%d", dstIP[0], dstIP[1], dstIP[2], dstIP[3])

			// 查询路由表找到目标 Peer
			peerID, err := h.lookupPeer(dstIPStr)
			if err != nil {
				h.Logger.Debugf("Could not find peer for %s: %s", dstIPStr, err)
				pkt.DecRef()
				continue
			}

			// 释放包引用
			pkt.DecRef()

			// 异步发送到对等节点
			go h.sendToPeer(ctx, pktData, peerID)
		}
	}
}

// lookupPeer 查询区块链获取 IP 对应的 PeerID
func (h *OutboundHandler) lookupPeer(ip string) (peer.ID, error) {
	value, found := h.Ledger.GetKey(protocol.MachinesLedgerKey, ip)
	if !found {
		return "", fmt.Errorf("IP %s not found in routing table", ip)
	}

	machine := &types.Machine{}
	if err := value.Unmarshal(machine); err != nil {
		return "", err
	}

	return peer.Decode(machine.PeerID)
}

// sendToPeer 发送 IP 包到对等节点
// 注意：必须与 TUN 模式的帧格式兼容（直接发送原始 IP 包，无前缀）
func (h *OutboundHandler) sendToPeer(ctx context.Context, pktData []byte, peerID peer.ID) {
	stream, err := h.Host.NewStream(ctx, peerID, protocol.EdgeVPN.ID())
	if err != nil {
		h.Logger.Debugf("Could not create stream to %s: %s", peerID, err)
		return
	}
	defer stream.Close()

	// 直接发送 IP 包数据（与 TUN 模式兼容）
	if _, err := stream.Write(pktData); err != nil {
		h.Logger.Debugf("Could not write packet to %s: %s", peerID, err)
		return
	}

	h.Logger.Debugf("Sent %d bytes to %s", len(pktData), peerID)
}
