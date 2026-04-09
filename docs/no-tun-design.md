# EdgeVPN No-TUN 模式设计方案（基于 gvisor/netstack）

## 一、需求分析

### 1.1 目标
在 EdgeVPN 的 VPN 模式中实现 `--no-tun` 选项，使得：
- **无需提权运行**：不依赖内核 TUN 设备，普通用户即可启动
- **保持兼容性**：其他客户端正常工作在 TUN 模式，可以正常访问 no-tun 节点
- **全协议支持**：完整支持 TCP/UDP/ICMP，应用完全透明
- **复用开源组件**：使用 `gvisor/netstack` 实现用户态协议栈

### 1.2 约束
- **不复用现有 Egress 设施**：仅对 VPN 服务做修改
- **保持现有 TUN 模式不变**：`--no-tun` 是可选模式，不影响现有功能
- **采用 Tailscale 方案**：使用 gvisor/netstack 替代内核协议栈

---

## 二、技术选型：为什么选择 gvisor/netstack

### 2.1 核心对比

| 对比项 | 纯 SOCKS5 代理 | gvisor/netstack |
|--------|---------------|-----------------|
| **应用透明性** | ❌ 需要配置代理环境变量 | ✅ 完全透明，标准网络调用即可 |
| **TCP 支持** | ✅ | ✅ |
| **UDP 支持** | ⚠️ 需要 UDP ASSOCIATE，支持有限 | ✅ 完整支持 |
| **ICMP 支持** | ❌ 不支持 | ✅ 自动响应 Ping |
| **实现复杂度** | 低 (~500 行) | 中高 (~2000 行) |
| **依赖大小** | ~50KB | ~8MB |
| **性能** | 高 | 中（用户态协议栈） |

**选择 netstack 的理由**：
1. **全协议支持**是核心需求
2. **应用透明**：用户无需修改应用或设置环境变量
3. **Tailscale 生产验证**：已在大规模使用
4. **Google 维护**：gvisor 是核心基础设施

---

## 三、整体架构设计

### 3.1 架构图

```
┌──────────────────────────────────────────────────────────────────────┐
│  EdgeVPN No-TUN 节点 (普通用户权限运行)                               │
│                                                                       │
│  本地应用 (curl, browser, ssh) ← 直接访问虚拟 IP，无需代理配置        │
│       │                                                               │
│       ▼                                                               │
│  ┌─────────────────────────────────────────────────────────────────┐ │
│  │  gvisor/netstack (用户态 TCP/IP 协议栈)                          │ │
│  │                                                                  │ │
│  │  - stack.Stack: TCP/UDP/ICMP/IPv4 完整协议栈                    │ │
│  │  - channel.Endpoint: 虚拟网卡（内存通道）                         │ │
│  │  - 路由表: 10.1.0.0/24 → NIC1                                   │ │
│  └────────────────────────┬────────────────────────────────────────┘ │
│                           │ IP 数据包                                 │
│                           ▼                                           │
│  ┌─────────────────────────────────────────────────────────────────┐ │
│  │  EdgeVPN P2P 适配层                                             │ │
│  │                                                                  │ │
│  │  outboundHandler ← 从 netstack 读包 → 查路由 → 发 libp2p 流    │ │
│  │  inboundHandler  ← 从 libp2p 流读包 → 注入 netstack            │ │
│  └────────────────────────┬────────────────────────────────────────┘ │
│                           │                                           │
│                           ▼                                           │
│  libp2p 网络层 (复用现有区块链、路由、加密)                           │
└──────────────────────────────────────────────────────────────────────┘
```

### 3.2 数据流

#### 出站：No-TUN 节点 → 远程节点
```
应用: curl http://10.1.0.50:8080
  ↓
netstack: TCP SYN → IP 包
  ↓
channel.Endpoint: 写入包
  ↓
outboundHandler:
  1. 读取 IP 包，解析目标 IP (10.1.0.50)
  2. 查询区块链: ledger.GetKey(MachinesLedgerKey, "10.1.0.50")
  3. 获取 PeerID_X
  4. 建立 libp2p 流: host.NewStream(ctx, PeerID_X, /edgevpn/0.1)
  5. 发送 IP 包
```

#### 入站：远程节点 → No-TUN 节点
```
TUN 节点发送 IP 包到 10.1.0.12
  ↓
查询区块链找到 PeerID_NoTUN
  ↓
建立 libp2p 流发送 IP 包
  ↓
inboundHandler:
  1. 从流读取 IP 包
  2. channel.Endpoint.InjectPacket(pkt)
  3. netstack 自动处理（TCP→应用 / UDP→应用 / ICMP→自动响应）
```

---

## 四、核心实现

### 4.1 新增文件清单

```
pkg/vpn/
├── netstack.go          # netstack 协议栈初始化和配置
├── netstack_link.go     # 虚拟网卡（channel.Endpoint）数据泵
├── netstack_outbound.go # 出站流量处理（netstack → libp2p）
└── netstack_inbound.go  # 入站流量处理（libp2p → netstack）
```

### 4.2 依赖库

```go
// go.mod 新增
require (
    gvisor.dev/gvisor v0.0.0-20240126212931-...  // netstack
)
```

### 4.3 核心代码实现

#### 4.3.1 netstack 协议栈初始化

```go
// pkg/vpn/netstack.go

package vpn

import (
    "gvisor.dev/gvisor/pkg/tcpip"
    "gvisor.dev/gvisor/pkg/tcpip/link/channel"
    "gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
    "gvisor.dev/gvisor/pkg/tcpip/stack"
    "gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
    "gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
    "gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// NetStack 封装 gvisor 协议栈
type NetStack struct {
    Stack  *stack.Stack
    LinkEP *channel.Endpoint
    NICID  tcpip.NICID
    LocalIP tcpip.Address
}

// NewNetStack 创建用户态网络栈
func NewNetStack(ip string, mtu int) (*NetStack, error) {
    // 1. 创建虚拟网卡
    linkEP := channel.New(1024, uint32(mtu), "")
    
    // 2. 创建协议栈
    s := stack.New(stack.Options{
        NetworkProtocols: []stack.NetworkProtocolFactory{
            ipv4.NewProtocol,
        },
        TransportProtocols: []stack.TransportProtocolFactory{
            tcp.NewProtocol,
            udp.NewProtocol,
            icmp.NewProtocol,
        },
        HandleLocal: true,
    })
    
    // 3. 注册 NIC
    nicID := tcpip.NICID(1)
    if err := s.CreateNIC(nicID, linkEP); err != nil {
        return nil, err
    }
    
    // 4. 添加 IP 地址
    localIP := tcpip.AddrFrom4([4]byte{/* 解析 ip */})
    s.AddAddress(nicID, ipv4.ProtocolNumber, localIP)
    
    // 5. 设置路由（整个 VPN 网络直连）
    subnet, _ := tcpip.NewSubnet(
        tcpip.AddrFrom4([4]byte{10, 1, 0, 0}),
        tcpip.MaskFrom("", 24),
    )
    s.AddRoute(tcpip.Route{
        Destination: subnet,
        Gateway:     "",
        NIC:         nicID,
    })
    
    return &NetStack{
        Stack:   s,
        LinkEP:  linkEP,
        NICID:   nicID,
        LocalIP: localIP,
    }, nil
}

func (ns *NetStack) Close() {
    ns.Stack.Close()
}
```

#### 4.3.2 出站流量处理

```go
// pkg/vpn/netstack_outbound.go

package vpn

import (
    "context"
    "gvisor.dev/gvisor/pkg/tcpip"
    "gvisor.dev/gvisor/pkg/tcpip/header"
    "github.com/libp2p/go-libp2p/core/network"
    "github.com/libp2p/go-libp2p/core/peer"
    "github.com/mudler/edgevpn/pkg/blockchain"
    "github.com/mudler/edgevpn/pkg/protocol"
)

// OutboundHandler 处理从 netstack 到 P2P 网络的流量
type OutboundHandler struct {
    Stack  *NetStack
    Ledger *blockchain.Ledger
    Host   host.Host
}

// Start 启动出站流量处理循环
func (h *OutboundHandler) Start(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
            // 从虚拟网卡读取 IP 包
            pkt, _, err := h.Stack.LinkEP.ReadContext(ctx)
            if err != nil {
                continue
            }
            
            // 解析 IP 包获取目标 IP
            pktBuf := pkt.NetworkHeader().Payload()
            ipHdr := header.IPv4(pktBuf)
            dstIP := ipHdr.DestinationAddress().String()
            
            // 查询路由表找到目标 Peer
            peerID, err := h.lookupPeer(dstIP)
            if err != nil {
                continue
            }
            
            // 建立 libp2p 流并发送
            go h.sendToPeer(ctx, pkt, peerID)
        }
    }
}

func (h *OutboundHandler) lookupPeer(ip string) (peer.ID, error) {
    value, found := h.Ledger.GetKey(protocol.MachinesLedgerKey, ip)
    if !found {
        return "", fmt.Errorf("IP %s not found", ip)
    }
    machine := &types.Machine{}
    value.Unmarshal(machine)
    return peer.Decode(machine.PeerID)
}

func (h *OutboundHandler) sendToPeer(ctx context.Context, pkt *stack.PacketBuffer, peerID peer.ID) {
    stream, err := h.Host.NewStream(ctx, peerID, protocol.EdgeVPN.ID())
    if err != nil {
        return
    }
    defer stream.Close()
    
    // 发送整个 IP 包（带长度前缀）
    pktData := pkt.NetworkHeader().View().AsSlice()
    stream.Write(lenBytes)  // 4 字节长度
    stream.Write(pktData)   // IP 包数据
}
```

#### 4.3.3 入站流量处理

```go
// pkg/vpn/netstack_inbound.go

package vpn

import (
    "encoding/binary"
    "github.com/libp2p/go-libp2p/core/network"
    "github.com/mudler/edgevpn/pkg/blockchain"
    "github.com/mudler/edgevpn/pkg/node"
)

// InboundStreamHandler 处理来自 P2P 网络的入站 IP 包
func InboundStreamHandler(l *blockchain.Ledger, ns *NetStack, nc node.Config) func(network.Stream) {
    return func(stream network.Stream) {
        defer stream.Close()
        
        // 验证 Peer 身份
        if !verifyPeer(stream, l, nc) {
            stream.Reset()
            return
        }
        
        // 持续读取流中的 IP 包
        for {
            // 读取长度前缀
            lenBytes := make([]byte, 4)
            if _, err := stream.Read(lenBytes); err != nil {
                return
            }
            pktLen := binary.BigEndian.Uint32(lenBytes)
            
            // 读取 IP 包
            pktData := make([]byte, pktLen)
            if _, err := stream.Read(pktData); err != nil {
                return
            }
            
            // 注入到 netstack
            ns.Stack.HandlePacket(
                ns.Stack.LinkEP.LinkAddress(),
                stack.NewPacketBuffer(stack.PacketBufferOptions{
                    Payload: pktData,
                }),
            )
        }
    }
}
```

#### 4.3.4 集成到 VPN 服务

```go
// pkg/vpn/vpn.go - 修改 VPNNetworkService

func VPNNetworkService(p ...Option) node.NetworkService {
    return func(ctx context.Context, nc node.Config, n *node.Node, b *blockchain.Ledger) error {
        c := &Config{...}
        c.Apply(p...)
        
        // 根据 NoTUN 标志选择模式
        if c.NoTUN {
            return runNetStackMode(ctx, c, n, b, nc)
        }
        
        // 原有 TUN 模式（保持不变）
        return runTUNMode(ctx, c, n, b, nc)
    }
}

func runNetStackMode(ctx context.Context, c *Config, n *node.Node, b *blockchain.Ledger, nc node.Config) error {
    // 1. 创建 netstack
    ns, err := NewNetStack(c.InterfaceAddress, c.InterfaceMTU)
    if err != nil {
        return err
    }
    defer ns.Close()
    
    // 2. 注册入站流处理器
    n.Host().SetStreamHandler(
        protocol.EdgeVPN.ID(),
        InboundStreamHandler(b, ns, nc),
    )
    
    // 3. 在区块链注册本机
    ip, _, _ := net.ParseCIDR(c.InterfaceAddress)
    b.Announce(ctx, c.LedgerAnnounceTime, func() {
        updatedMap := map[string]interface{}{}
        updatedMap[ip.String()] = newBlockChainData(n, ip.String())
        b.Add(protocol.MachinesLedgerKey, updatedMap)
    })
    
    // 4. 启动出站处理
    outbound := &OutboundHandler{
        Stack:  ns,
        Ledger: b,
        Host:   n.Host(),
    }
    go outbound.Start(ctx)
    
    c.Logger.Info("No-TUN mode started")
    <-ctx.Done()
    return nil
}
```

### 4.5 配置扩展

```go
// pkg/vpn/config.go

type Config struct {
    // 现有字段...
    NoTUN bool  // 启用 userspace networking
}

func WithNoTUN(enabled bool) Option {
    return func(c *Config) error {
        c.NoTUN = enabled
        return nil
    }
}
```

```go
// cmd/main.go - 新增 CLI flag

&cli.BoolFlag{
    Name:    "no-tun",
    Usage:   "Run in userspace networking mode (no TUN device required)",
    EnvVars: []string{"EDGEVPN_NO_TUN"},
},
```

---

## 五、使用示例

```bash
# 生成配置
edgevpn -g -b > token.txt

# TUN 模式（默认，需要提权）
sudo EDGEVPNTOKEN=$(cat token.txt) edgevpn --address 10.1.0.11/24

# No-TUN 模式（无需提权）
EDGEVPNTOKEN=$(cat token.txt) edgevpn --address 10.1.0.12/24 --no-tun

# 测试连通性
ping 10.1.0.11          # ICMP
curl http://10.1.0.11:8080  # HTTP
ssh 10.1.0.11           # SSH
```

---

## 六、技术难点与解决方案

### 6.1 性能优化
- **问题**：gvisor netstack 在用户态，性能低于内核
- **解决**：
  - 调整 channel.Endpoint 队列大小
  - 使用零拷贝优化包传递
  - 启用 libp2p YAMUX 多路复用

### 6.2 内存管理
- **问题**：gvisor 依赖 Bazel，Go modules 集成复杂
- **解决**：
  - 使用 `gvisor.dev/gvisor` 官方 Go branch
  - 或使用 `noisysockets/netstack`（提取版）

### 6.3 包格式转换
- **问题**：netstack 使用自定义 PacketBuffer，需正确转换
- **解决**：
  - 仔细处理 `stack.NewPacketBuffer()`
  - 参考 wireguard-go/tun/netstack 实现

### 6.4 路由表同步
- **问题**：区块链路由表变化需更新到 netstack
- **解决**：
  - 监听区块链更新事件
  - 动态调用 `s.AddRoute()` 添加路由

---

## 七、与 Tailscale/EasyTier 对比

| 特性 | Tailscale (Userspace) | EasyTier (No-TUN) | EdgeVPN (No-TUN) |
|------|----------------------|-------------------|------------------|
| **核心技术** | gvisor/netstack | Rust/Tokio + SOCKS5 | gvisor/netstack |
| **TCP** | ✅ | ✅ | ✅ |
| **UDP** | ✅ | ⚠️ | ✅ |
| **ICMP** | ✅ | ✅ | ✅ |
| **应用透明** | ✅ | ❌ 需配代理 | ✅ |
| **依赖大小** | ~10MB | ~2MB | ~8MB |
| **P2P 传输** | WireGuard+UDP | 自定义 TCP | libp2p |

---

## 八、实现路线图

### Phase 1: netstack 集成（2-3 天）
1. 添加 gvisor 依赖到 go.mod
2. 实现 `NewNetStack()` 创建协议栈
3. 验证 channel.Endpoint 读写

### Phase 2: P2P 适配层（2-3 天）
4. 实现 OutboundHandler
5. 实现 InboundStreamHandler
6. 集成到 VPNNetworkService

### Phase 3: CLI 和配置（1 天）
7. 添加 `--no-tun` flag
8. 更新 Config 结构

### Phase 4: 测试（2 天）
9. 单元测试：netstack 创建、包转发
10. 集成测试：TUN ↔ No-TUN 通信
11. 手动测试：ping/curl/ssh

---

## 九、总结

采用 Tailscale 的 **gvisor/netstack** 方案，EdgeVPN No-TUN 模式能够：

✅ **全协议支持**：TCP/UDP/ICMP 完整支持  
✅ **应用透明**：无需配置代理，标准网络调用即可  
✅ **无需提权**：普通用户权限运行  
✅ **完全兼容**：TUN 模式客户端正常访问  
✅ **生产验证**：Tailscale 已大规模使用  

**代价**：
- 依赖增加 ~8MB
- 实现复杂度增加 ~2000 行代码
- 性能略低于内核 TUN 模式

这个方案在 **功能完整性** 和 **实现复杂度** 之间取得了最佳平衡。
