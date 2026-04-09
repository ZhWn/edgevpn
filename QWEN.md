# EdgeVPN - QWEN Context

## Project Overview

**EdgeVPN** 是一个完全去中心化、不可变、可移植的静态编译 VPN 和反向代理工具，基于 p2p 网络构建。它使用 `libp2p` 库创建私有去中心化网络，通过共享密钥（token）进行访问。

### 核心功能

- **VPN**: 在 p2p 节点之间建立安全的 VPN 连接
  - 自动为节点分配 IP 地址
  - 内置小型 DNS 服务器，支持解析内外网 IP
  - 创建可信区域（Trust Zones），防止 token 泄露后的网络访问
  
- **反向代理**: 类似 `ngrok` 的功能，通过 p2p 网络暴露 TCP 服务
  
- **文件传输**: 无需建立 VPN 连接即可通过 p2p 发送文件
  
- **区块链**: 内置分布式账本功能
  
- **库模式**: 可作为 Go 库集成到其他项目中

### 技术栈

- **语言**: Go 1.23+ (toolchain: go1.24.1)
- **核心依赖**:
  - `go-libp2p` - p2p 网络通信
  - `go-libp2p-pubsub` - 发布/订阅消息
  - `go-libp2p-kad-dht` - 分布式哈希表
  - `water` - TUN/TAP 设备支持
  - `echo/v4` - HTTP API 框架
  - `dns` (miekg/dns) - DNS 服务器
- **许可证**: Apache License v2

## Project Structure

```
edgevpn/
├── main.go              # 入口文件，CLI 应用定义
├── cmd/                 # CLI 命令实现
│   ├── main.go          # 主命令（VPN 启动）
│   ├── api.go           # API 服务命令
│   ├── dns.go           # DNS 相关命令
│   ├── file.go          # 文件收发命令
│   ├── join.go          # 加入网络命令
│   ├── proxy.go         # 代理命令
│   ├── service.go       # 服务管理命令
│   └── peergate.go      # 节点门控命令
├── pkg/                 # 核心库
│   ├── node/            # 节点管理（核心）
│   ├── vpn/             # VPN 功能
│   ├── blockchain/      # 区块链实现
│   ├── crypto/          # 加密模块（AES、Sealer）
│   ├── discovery/       # 节点发现
│   ├── services/        # 服务模块（DNS、Egress、Alive）
│   ├── protocol/        # 协议定义
│   ├── stream/          # 流处理
│   ├── hub/             # 连接中心
│   ├── config/          # 配置管理
│   ├── types/           # 类型定义
│   ├── utils/           # 工具函数
│   └── trustzone/       # 可信区域
├── api/                 # HTTP API 服务
│   ├── api.go           # API 实现
│   ├── client/          # API 客户端
│   ├── generate/        # 代码生成
│   ├── public/          # 静态资源（Web 界面）
│   └── types/           # API 类型定义
├── internal/            # 内部版本信息
├── docs/                # 文档（Hugo 静态站点）
├── scripts/             # 辅助脚本
└── .github/             # GitHub Actions CI/CD
```

## Building and Running

### 环境要求

- Go 1.23+
- 支持的平台: Linux, Windows, Darwin (macOS), FreeBSD
- 支持的架构: amd64, arm, 386, arm64

### 构建

```bash
# 标准构建
go build -o edgevpn

# 带版本信息的构建
go build -ldflags="-s -w -X github.com/mudler/edgevpn/internal.Version=<tag> -X github.com/mudler/edgevpn/internal.Commit=<commit>" -o edgevpn

# Docker 构建
docker build -t edgevpn .
```

### 使用 GoReleaser 构建（发布用）

```bash
goreleaser build --clean --snapshot
```

### 运行

#### 生成配置文件

```bash
# 生成 YAML 配置
edgevpn -g > config.yaml

# 生成 Base64 Token
edgevpn -g -b
```

#### 启动 VPN

```bash
# 使用 token 启动
EDGEVPNTOKEN=<token> edgevpn --address 10.1.0.11/24

# 使用配置文件启动
EDGEVPNCONFIG=config.yaml edgevpn --address 10.1.0.11/24

# 启用 API 和 DHCP
EDGEVPNCONFIG=config.yaml edgevpn --address 10.1.0.11/24 --api --dhcp
```

#### 启动 API 服务

```bash
edgevpn api --listen 127.0.0.1:8080
```

#### 文件传输

```bash
# 发送文件
edgevpn filesend <file>

# 接收文件
edgevpn filereceive
```

### 测试

```bash
# 运行单元测试
go test ./...

# 运行测试（带覆盖率）
go test -coverprofile=coverage.txt ./...

# 运行特定测试包
go test ./pkg/node/
go test ./api/
```

测试使用 Ginkgo/Gomega BDD 框架。

### Docker 运行

```bash
docker run --rm -it edgevpn -g
```

## Development Conventions

### 代码风格

- 遵循 Go 标准代码格式
- 使用 `go fmt` 格式化代码
- 使用 `goimports` 管理导入
- 所有源文件包含 Apache License 头部注释

### 测试实践

- 使用 Ginkgo v2 + Gomega 进行 BDD 风格测试
- 测试文件命名: `*_test.go`
- 测试套件组织: `*_suite_test.go`
- 测试脚本位于 `.github/tests.sh`, `.github/vpntest.sh` 等

### CLI 框架

- 使用 `urfave/cli/v2` 构建命令行界面
- 支持环境变量配置（如 `EDGEVPNTOKEN`, `EDGEVPNCONFIG`, `ADDRESS` 等）
- 子命令模式: `start`, `api`, `proxy`, `filesend`, `filereceive`, `dns` 等

### 配置管理

配置可以通过以下方式提供:
1. 环境变量（优先）
2. CLI 参数
3. YAML 配置文件
4. Base64 编码的 token

### 网络性能调优

如果遇到网络性能问题，建议增加系统缓冲区大小:

```bash
sudo sysctl -w net.core.rmem_max=2500000
```

## CI/CD

项目使用 GitHub Actions:

- **build.yml**: 使用 GoReleaser 构建多平台二进制文件
- **test.yml**: 运行单元测试和集成测试（VPN、服务、文件传输）
- **release.yml**: 发布自动化
- **images.yml**: Docker 镜像构建
- **pages.yml**: 文档站点部署
- **dependabot.yml**: 依赖更新

## Key Components

### 节点（Node）

核心组件，位于 `pkg/node/`，负责:
- p2p 网络连接和发现
- 区块链同步
- 加密通信
- 生命周期管理

### VPN 模块

位于 `pkg/vpn/`，实现:
- TUN/TAP 接口管理
- IP 地址分配（静态/DHCP）
- 数据包路由和转发

### API 服务

位于 `api/`，提供:
- RESTful API
- Web 仪表盘（深色/浅色主题）
- 带宽统计和监控
- 节点状态查询

## Common Environment Variables

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `EDGEVPNTOKEN` | 网络 token (Base64) | - |
| `EDGEVPNCONFIG` | 配置文件路径 | - |
| `ADDRESS` | VPN 虚拟地址 | `10.1.0.1/24` |
| `IFACE` | 网络接口名称 | `edgevpn0` |
| `API` | 是否启动 API | `false` |
| `APILISTEN` | API 监听地址 | `127.0.0.1:8080` |
| `DHCP` | 启用 DHCP | `false` |
| `DHCPLEASEDIR` | DHCP 租约目录 | `~/.edgevpn/leases` |
| `DNSADDRESS` | DNS 监听地址 | - |
| `DNSFORWARD` | 启用 DNS 转发 | `true` |
| `EGRESS` | 启用出口路由 | `false` |

## Important Notes

1. **安全性**: 此软件未经完整安全审计，不建议用于生产环境或敏感流量
2. **网络特性**: 去中心化网络具有"高聊天性"（chatty），使用 Gossip 协议同步路由表
3. **性能**: 可能不适合低延迟工作负载
4. **Token 安全**: Token/配置文件等同于网络完全控制权，需妥善保管
