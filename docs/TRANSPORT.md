# Limesh 传输协议

> 本文档是 Limesh 传输层的详解手册,面向需要理解选型逻辑或扩展传输层的开发者。
>
> 技术方案总纲见 [AGENTS.md](../AGENTS.md) 第 9 章,配置参考见 [CONFIG.md](CONFIG.md),用户入门见 [README.md](../README.md)。

---

## 目录

1. [概述](#1-概述)
2. [统一接口](#2-统一接口)
3. [传输协议列表](#3-传输协议列表)
4. [逐个详解](#4-逐个详解)
5. [选型决策树](#5-选型决策树)
6. [能力矩阵](#6-能力矩阵)
7. [性能对比](#7-性能对比)
8. [扩展指南](#8-扩展指南)
9. [常见问题](#9-常见问题)

---

## 1. 概述

传输层(L3)的职责只有一个:**把 ZCPacket 的字节从一个节点搬到另一个节点**。

它不解析 IP 包、不做路由决策、不参与 ACL 判定。这些属于上层(L4 数据面与 L5 路由层)。传输层唯一需要额外表达的信息是它的**能力**(可靠?保序?能否复用连接?建议 MTU 多少?),数据面据此决定要不要叠加重传、重排或分片。

这种切分带来一个重要后果:**新增一种传输不需要改动路由、加密或 Peer 状态机**。传输是可插拔的,这是 18 种协议能够共存而不失控的前提。

### 1.1 为什么需要这么多种传输

不是为了堆数量,而是为了应对**真实网络环境的多样性**:

| 受限环境 | 可用传输 |
|---|---|
| 正常公网 | UDP、QUIC |
| UDP 被 QoS 或被封锁 | TCP、KCP |
| 只放行 HTTP/HTTPS | WS、WSS、TLS、HTTP/2 |
| 只放行 443 | WSS、HTTP/3 |
| 只放行 22 | SSH |
| 只放行 ICMP | ICMP |
| 只放行 53 | DNS |
| 企业代理 | HTTP/2、gRPC、WSS |
| 同主机 | 共享内存、Unix Socket |
| 虚拟机 ↔ 宿主 | VSOCK |
| 已有 WireGuard 网络 | WireGuard |

在这些环境里,"支持多少协议"直接决定了"能不能连通"。一个只支持 UDP 的组网工具在限制严格的运营商网络里可能完全不可用,而这不是配置问题,是能力问题。

### 1.2 设计约束

| 约束 | 说明 |
|---|---|
| 纯 Go | `CGO_ENABLED=0`,任何传输实现不得引入 C 依赖 |
| 接口统一 | 所有传输实现同一个 `Transport` 接口 |
| 能力自述 | 通过 `Caps` 声明自身能力,不靠类型断言猜 |
| 可注册 | 新增协议只需注册工厂函数,不改核心代码 |
| 许可证兼容 | 依赖必须与 Apache 2.0 兼容 |

---

## 2. 统一接口

### 2.1 Transport

```go
// Transport 是所有传输协议必须实现的接口。
type Transport interface {
    // Name 返回注册名,与配置中的 transports.protocols 项一一对应。
    Name() string
    // Dial 主动连接到指定对端。
    Dial(ctx context.Context, peer PeerAddr) (Conn, error)
    // Listen 监听入站连接。
    Listen(ctx context.Context, addr string) (Listener, error)
    // Capabilities 声明该传输的能力(可靠性、是否支持多路复用等)。
    Capabilities() Caps
}
```

### 2.2 Caps

`Caps` 至少包含以下字段:

```go
// Caps 描述一种传输的能力,数据面据此决定封装策略。
type Caps struct {
    // Reliable 表示该传输自带重传与拥塞控制。
    // 为 true 时数据面不再叠加确认机制。
    Reliable bool
    // Ordered 表示该传输保证字节顺序。
    // 为 false 时数据面必须启用序号重排。
    Ordered bool
    // Multiplexed 表示该传输原生支持多路复用。
    // 为 true 时多个 Peer 可共用一个底层连接。
    Multiplexed bool
    // MTU 是该传输在典型路径上的建议最大载荷。
    // 数据面据此决定是否分片。
    MTU int
}
```

数据面依据 `Caps` 做三件事:

| 字段 | 数据面的决策 |
|---|---|
| `Reliable` | `false` → 控制消息(LSA/Close)启用应用层确认与重传 |
| `Ordered` | `false` → 启用接收端序号重排缓冲 |
| `Multiplexed` | `false` → 每个 Peer 独立建连;`true` → 复用底层连接 |
| `MTU` | 决定分片阈值;小于 ZCPacket 头 + 载荷时强制分片 |

**这是设计上的关键点**:传输只声明事实,不决定策略。策略集中在上层,因此换传输不会改变上层的可靠性语义。

### 2.3 Conn

```go
// Conn 是一条已建立的双向传输连接。
type Conn interface {
    // Read 读取一个载荷。返回的切片在下次 Read 前有效。
    Read(p []byte) (n int, err error)
    // Write 写入一个载荷。实现不得在返回后继续引用 p。
    Write(p []byte) (n int, err error)
    // Close 关闭连接。
    Close() error
    // LocalAddr 返回本地地址,用于候选地址收集。
    LocalAddr() net.Addr
    // RemoteAddr 返回对端地址,用于候选地址收集与路径展示。
    RemoteAddr() net.Addr
    // Stats 返回该连接的实时统计。
    Stats() ConnStats
}

// ConnStats 是连接级统计,用于观测与路由打分。
type ConnStats struct {
    RTT        time.Duration // 平滑往返延迟
    Loss       float64       // 丢包率 [0,1]
    BytesSent  uint64
    BytesRecv  uint64
    Retransmit uint64 // 传输层自身的重传次数
}
```

### 2.4 Listener

```go
// Listener 接受入站连接。
type Listener interface {
    // Accept 阻塞等待一条入站连接。
    Accept(ctx context.Context) (Conn, error)
    // Addr 返回监听地址。
    Addr() net.Addr
    // Close 停止监听。
    Close() error
}
```

### 2.5 Registry

传输通过注册表管理:

```go
// Register 注册一种传输实现。在包的 init 中调用。
func Register(name string, factory Factory)

// Factory 构造一个传输实例。
type Factory func(cfg TransportConfig) (Transport, error)

// Get 按名称查找传输。
func Get(name string) (Transport, bool)
```

注册示例:

```go
package udp

import "github.com/LiStudioorg/limesh/internal/transport"

func init() {
    transport.Register("udp", func(cfg transport.TransportConfig) (transport.Transport, error) {
        return New(cfg), nil
    })
}
```

注册名与配置中的 `transports.protocols` 项**一一对应**:

```toml
[transports]
protocols = ["udp", "quic", "tcp"]
```

---

## 3. 传输协议列表

| # | 协议 | 注册名 | 承载 | 特点 | 主要场景 |
|---|---|---|---|---|---|
| 1 | UDP | `udp` | UDP | 延迟最低,无重传 | 默认首选,公网直连 |
| 2 | TCP | `tcp` | TCP | 穿透友好,可靠 | UDP 被封锁时的兜底 |
| 3 | QUIC | `quic` | UDP | 加密 + 多路复用 + 0-RTT | 高丢包广域网 |
| 4 | TLS | `tls` | TCP | 伪装成 HTTPS 流量 | 严格防火墙环境 |
| 5 | WS | `ws` | TCP | WebSocket,握手像 HTTP | 仅放行 HTTP 的代理环境 |
| 6 | WSS | `wss` | TLS | WebSocket over TLS | 企业代理 + 加密 |
| 7 | HTTP/2 | `http2` | TCP/TLS | 多路复用 + 头部压缩 | 与现有 HTTP/2 基建共存 |
| 8 | HTTP/3 | `http3` | QUIC | 基于 QUIC 的 HTTP | 弱网 + HTTP 基建 |
| 9 | gRPC | `grpc` | HTTP/2 | 双向流 + 强类型 | 与云原生控制面集成 |
| 10 | KCP | `kcp` | UDP | 激进重传,弱网吞吐好 | 高丢包跨境链路 |
| 11 | DTLS | `dtls` | UDP | 数据报 TLS | 需要 TLS 语义但不想用 TCP |
| 12 | WireGuard | `wireguard` | UDP | 与 WG 生态互操作 | 与现有 WG 网络对接 |
| 13 | SSH | `ssh` | TCP | 复用 22 端口 | 只开放 SSH 的受限环境 |
| 14 | ICMP | `icmp` | ICMP | 伪装成 ping | 只放行 ICMP 的网络 |
| 15 | DNS | `dns` | UDP/TCP | 伪装成 DNS 查询 | 只放行 53 端口的网络 |
| 16 | Unix Socket | `unix` | 本地 | 零网络栈开销 | 同机多进程互联 |
| 17 | VSOCK | `vsock` | 本地 | 虚拟机 ↔ 宿主 | VM/容器沙箱场景 |
| 18 | 共享内存 | `shm` | 本地 | 延迟最低 | 同机高频通信 |

### 3.1 分类

按部署位置与用途分三类:

**公网原生传输**(1–3):直接利用公网 UDP/TCP 能力,性能最好。

```
UDP · TCP · QUIC
```

**伪装与隧道传输**(4–15):把流量包装成另一种协议,用于绕过网络限制。

```
TLS · WS · WSS · HTTP/2 · HTTP/3 · gRPC · KCP · DTLS · WireGuard · SSH · ICMP · DNS
```

**本地传输**(16–18):不经过网络栈,用于同主机或虚拟机场景。

```
Unix Socket · VSOCK · 共享内存
```

---

## 4. 逐个详解

### 4.1 UDP

**定位**:默认首选。公网环境下延迟最低、开销最小。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | 标准库 `net` |
| `Caps.Reliable` | `false` |
| `Caps.Ordered` | `false` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1380 |
| 默认端口 | 11010 |

UDP 传输直接使用 `net.UDPConn`,每个 Peer 一个 socket。由于 ZCPacket 头自带 `Sequence` 与 `Ack`,UDP 的无序不可靠特性不影响控制消息的正确性——控制消息在上层有显式确认。

**配置**

```toml
[transports]
protocols = ["udp"]

[listen]
port = 11010
```

**优点**

- 延迟最低,无队头阻塞。
- 开销最小,无额外握手。
- 是打洞能力最强的承载(NAT 映射直接对应)。

**缺点**

- 无重传,弱网下丢包直接体现为上层重传(TCP)或数据丢失(UDP)。
- 部分运营商与企业网络会限制或 QoS 大流量 UDP。
- 需要防火墙同时放行 UDP 与 TCP。

---

### 4.2 TCP

**定位**:兜底传输。UDP 被封锁时的第一备选。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | 标准库 `net` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1380(受 MSS 影响) |
| 默认端口 | 11010 |

TCP 天然可靠保序,因此数据面**不再叠加**确认机制,控制消息直接复用 TCP 的可靠性。

需要注意 TCP-over-TCP 问题:在丢包链路上,外层 TCP 与内层 TCP 各自重传,会放大重传量。因此 Limesh 在 `Caps.Reliable = true` 的传输上关闭应用层重传,避免双重可靠性。

**配置**

```toml
[transports]
protocols = ["tcp"]
```

**优点**

- 穿透性最好,几乎所有防火墙默认放行。
- 自带可靠保序,实现简单。
- 配合 TCP 同时打开(Simultaneous Open)可用于部分 NAT 场景的打洞。

**缺点**

- 队头阻塞:一个包丢失会阻塞后续所有数据。
- TCP-over-TCP 在丢包链路上性能明显下降。
- 延迟通常高于 UDP。

---

### 4.3 QUIC

**定位**:高丢包广域网的最佳选择。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `quic-go` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true`(按流) |
| `Caps.Multiplexed` | `true` |
| `Caps.MTU` | 1350 |
| 默认端口 | 11010/udp |

QUIC 把可靠性与加密内置到协议里,并且是**按流**而非按连接处理丢包,因此避免了 TCP 的队头阻塞。0-RTT 握手让重连几乎没有延迟代价。

由于 `Caps.Multiplexed = true`,数据面会让多个 Peer 复用同一条 QUIC 连接,减少 socket 数量与握手次数。

**配置**

```toml
[transports]
protocols = ["quic"]

[transports.quic]
max_streams = 256
```

**优点**

- 无队头阻塞,弱网下明显优于 TCP。
- 原生加密与多路复用。
- 0-RTT 重连,建连快。
- 基于 UDP,保留良好的打洞能力。

**缺点**

- 部分网络对 UDP 大流量有 QoS。
- 实现复杂度高于 TCP,资源占用略高。
- 某些中间设备对 QUIC 的处理不完善。

---

### 4.4 TLS

**定位**:把流量伪装成标准 HTTPS,用于严格防火墙环境。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | 标准库 `crypto/tls` |
| `Caps.Reliable` | `true`(基于 TCP) |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1380 |
| 默认端口 | 443(可配置) |

传输层 TLS 与 Limesh 自身的 Noise E2EE 是**两层独立加密**。外层 TLS 的目的是让流量在中间设备看来就是普通 HTTPS,内层 Noise 保证端到端安全——即使外层 TLS 在某处被解密,payload 仍是密文。

**配置**

```toml
[transports]
protocols = ["tls"]

[transports.tls]
port = 443
cert = "/etc/mesh/tls/cert.pem"
key  = "/etc/mesh/tls/key.pem"
sni  = "cdn.example.com"
```

**优点**

- 伪装效果好,SNI 可指向常见域名。
- 几乎所有网络都放行 443。

**缺点**

- 需要证书(自签亦可,但更容易被识别)。
- 基于 TCP,有队头阻塞。
- 双层加密带来额外 CPU 开销。

---

### 4.5 WS(WebSocket)

**定位**:仅放行 HTTP 的代理环境。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `gorilla/websocket` 或 `nhooyr.io/websocket` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1380 |
| 默认端口 | 80 |

握手阶段是标准 HTTP Upgrade 请求,因此可以穿过只允许 HTTP 的代理与防火墙。配置中可自定义路径与 Host 头以进一步拟态。

**配置**

```toml
[transports]
protocols = ["ws"]

[transports.ws]
port = 80
path = "/ws"
host = "cdn.example.com"
```

**优点**

- 握手像普通 HTTP,穿透代理能力强。
- 明文传输,便于调试(但仍受内层 Noise 保护)。

**缺点**

- 明文握手特征明显,深度包检测环境可能识别。
- 基于 TCP,有队头阻塞。
- 需要中间设备支持 HTTP Upgrade。

---

### 4.6 WSS(WebSocket over TLS)

**定位**:企业代理 + 加密场景的默认选择。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `gorilla/websocket` + `crypto/tls` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1380 |
| 默认端口 | 443 |

WSS 是 WS 的 TLS 版本,也是实际生产中最常用的隧道传输。它同时具备"看起来像 HTTPS"与"内容被加密"两个特性。

**配置**

```toml
[transports]
protocols = ["wss"]

[transports.wss]
port = 443
path = "/ws"
host = "cdn.example.com"
cert = "/etc/mesh/tls/cert.pem"
key  = "/etc/mesh/tls/key.pem"
```

**优点**

- 伪装效果好,与普通 HTTPS 流量难以区分。
- 443 端口几乎总是放行。
- 企业代理普遍支持。

**缺点**

- 基于 TCP,有队头阻塞。
- 双层加密的 CPU 开销。
- 需要维护证书。

---

### 4.7 HTTP/2

**定位**:与现有 HTTP/2 基础设施共存。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `golang.org/x/net/http2` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true`(按流) |
| `Caps.Multiplexed` | `true` |
| `Caps.MTU` | 1380 |
| 默认端口 | 443 |

HTTP/2 的多路复用让多个 Peer 共享一条连接,同时头压缩降低了小包开销。

**配置**

```toml
[transports]
protocols = ["http2"]

[transports.http2]
port = 443
path = "/mesh"
```

**优点**

- 多路复用,连接数少。
- 头压缩对小包友好。
- 与现有反向代理(nginx、Envoy)兼容性最好。

**缺点**

- 基于 TCP,TCP 层丢包仍会阻塞所有流。
- 协议复杂度高,调试困难。

---

### 4.8 HTTP/3

**定位**:弱网 + HTTP 基础设施的组合。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `quic-go/http3` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true`(按流) |
| `Caps.Multiplexed` | `true` |
| `Caps.MTU` | 1350 |
| 默认端口 | 443/udp |

HTTP/3 建在 QUIC 之上,因此继承 QUIC 的无队头阻塞与 0-RTT,同时保留 HTTP 的语义与头部。

**配置**

```toml
[transports]
protocols = ["http3"]

[transports.http3]
port = 443
path = "/mesh"
```

**优点**

- 兼顾 HTTP 兼容性与 QUIC 的弱网性能。
- 多路复用 + 无队头阻塞。

**缺点**

- UDP 大流量可能被 QoS。
- 中间设备对 HTTP/3 支持参差不齐。

---

### 4.9 gRPC

**定位**:与云原生控制面集成,双向流。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `google.golang.org/grpc` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `true` |
| `Caps.MTU` | 1380 |
| 默认端口 | 443 |

gRPC 的双向流语义与 ZCPacket 的需求天然契合。使用 gRPC 传输时,控制面与数据面可以复用同一套服务定义。

**配置**

```toml
[transports]
protocols = ["grpc"]

[transports.grpc]
port = 443
```

**优点**

- 双向流,语义清晰。
- 与 K8s、服务网格生态集成度高。
- 强类型接口,不易出错。

**缺点**

- 依赖链较重,二进制体积增加。
- 基于 HTTP/2,继承其 TCP 层队头阻塞。

---

### 4.10 KCP

**定位**:高丢包跨境链路的吞吐优化。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `xtaci/kcp-go` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1380 |
| 默认端口 | 11010/udp |

KCP 是"用带宽换延迟"的可靠 UDP 协议。它比 TCP 更激进地重传,因此在高丢包链路上延迟与吞吐都明显更好,代价是带宽放大(见[性能对比](#7-性能对比))。

`fast` 模式进一步降低延迟,适合交互式场景;`normal` 模式更温和,适合大流量传输。

**配置**

```toml
[transports]
protocols = ["kcp"]

[transports.kcp]
mode   = "fast"
mtu    = 1380
window = 512
```

**优点**

- 高丢包环境下吞吐显著优于 TCP。
- 延迟可控(`fast` 模式)。
- 基于 UDP,保留打洞能力。

**缺点**

- 带宽放大明显(实测可达 1.3~1.5 倍)。
- 参数调优需要经验。
- 激进重传可能对共享链路造成压力。

---

### 4.11 DTLS

**定位**:需要 TLS 语义但不想用 TCP。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `pion/dtls` |
| `Caps.Reliable` | `false` |
| `Caps.Ordered` | `false` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1350 |
| 默认端口 | 11010/udp |

DTLS 是 TLS 的数据报版本,保留 TLS 的握手与加密,但运行在 UDP 上,因此没有队头阻塞。

**配置**

```toml
[transports]
protocols = ["dtls"]

[transports.dtls]
cert = "/etc/mesh/tls/cert.pem"
key  = "/etc/mesh/tls/key.pem"
```

**优点**

- 有 TLS 语义,但无 TCP 的队头阻塞。
- 适合实时性要求高的场景。

**缺点**

- 基于 UDP,受 UDP 限制影响。
- 库生态不如 TLS 成熟。

---

### 4.12 WireGuard

**定位**:与现有 WireGuard 网络互操作。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `golang.zx2c4.com/wireguard` |
| `Caps.Reliable` | `false` |
| `Caps.Ordered` | `false` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1420 |
| 默认端口 | 51820/udp |

WireGuard 传输用于把 Limesh 接入已有的 WG 网络,或让 WG 客户端通过 Limesh 组网。安全模型与 Limesh 自身的 Noise IK 属于同一族,因此可以安全叠加。

> 注意:使用 WireGuard 传输时存在两层独立密钥体系(Limesh 的 Noise 与 WG 的 Noise),MTU 需要相应下调以避免分片。

**配置**

```toml
[transports]
protocols = ["wireguard"]

[transports.wireguard]
private_key = "/etc/mesh/wg/private.key"
peer_key    = "base64-public-key="
endpoint    = "vpn.example.com:51820"
```

**优点**

- 与 WG 生态无缝互操作。
- 性能优秀,WG 实现成熟。
- 与 Limesh 的安全模型兼容。

**缺点**

- 需要管理第二套密钥。
- MTU 需要额外下调。
- 与 WG 的 AllowedIPs 语义存在概念映射成本。

---

### 4.13 SSH

**定位**:只开放 22 端口的受限环境。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `golang.org/x/crypto/ssh` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `true` |
| `Caps.MTU` | 1380 |
| 默认端口 | 22 |

SSH 传输把 ZCPacket 承载在 SSH 通道上。由于几乎每台服务器都开放 22 端口,这是**可用性最高的隧道方案之一**,但性能代价明显。

**配置**

```toml
[transports]
protocols = ["ssh"]

[transports.ssh]
port        = 22
user        = "mesh"
private_key = "/root/.ssh/id_ed25519"
```

**优点**

- 22 端口几乎总是开放。
- 复用成熟 SSH 生态(密钥、认证、审计)。
- `Caps.Multiplexed = true`,多 Peer 共享一条连接。

**缺点**

- 吞吐低,明显劣于原生 UDP。
- 依赖 SSH 服务端配置。
- 加密层数多,CPU 开销大。

---

### 4.14 ICMP

**定位**:只放行 ICMP 的网络,**最后手段**。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `golang.org/x/net/icmp` |
| `Caps.Reliable` | `false` |
| `Caps.Ordered` | `false` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1200(保守值) |
| 权限 | 需要 `CAP_NET_RAW` |

ICMP 传输把数据封装在 ICMP Echo 报文里,使流量在中间设备看来就是普通 `ping`。

**配置**

```toml
[transports]
protocols = ["icmp"]

[transports.icmp]
interval = "100ms"
```

**优点**

- 在极端受限网络中唯一可行的方案。
- ICMP 通常不被防火墙深度检查。

**缺点**

- **吞吐极低**(受 ping 频率与包大小限制)。
- 需要 `CAP_NET_RAW` 或 root。
- 大量 ICMP 流量容易被网络管理员注意。
- 多数系统对 ICMP 速率有限制。

**仅在其他协议全部不可用时使用。**

---

### 4.15 DNS

**定位**:只放行 53 端口的网络,**最后手段**。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `golang.org/x/net/dns/dnsmessage` |
| `Caps.Reliable` | `false`(UDP 模式)/ `true`(TCP 模式) |
| `Caps.Ordered` | `false`(UDP 模式)/ `true`(TCP 模式) |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 1200 |
| 默认端口 | 53 |

DNS 传输把数据伪装成 DNS 查询与响应。由于 53 端口在几乎所有网络中都开放,可用性高,但吞吐受限于 DNS 报文大小与查询频率。

**配置**

```toml
[transports]
protocols = ["dns"]

[transports.dns]
port   = 53
proto  = "udp"          # udp / tcp
domain = "t.example.com"
```

**优点**

- 53 端口极其常见地开放。
- 伪装效果好。

**缺点**

- **吞吐极低**。
- 需要控制一个域名用于编码数据。
- 异常的 DNS 流量特征容易被检测系统识别。
- 依赖 DNS 解析器的行为(缓存、重试),可能造成抖动。

**仅在其他协议全部不可用时使用。**

---

### 4.16 Unix Socket

**定位**:同机多进程互联。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | 标准库 `net`(`unix` 网络) |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 65536 |
| 默认路径 | `/var/run/mesh/mesh.sock` |

Unix Socket 完全绕过网络栈,没有 IP 封装开销,延迟与吞吐都远优于任何网络传输。

**配置**

```toml
[transports]
protocols = ["unix"]

[transports.unix]
path = "/var/run/mesh/mesh.sock"
mode = "0600"
```

**优点**

- 零网络栈开销,性能最好。
- 通过文件权限即可控制访问(无需额外 ACL)。
- 不占用端口,不与其它服务冲突。

**缺点**

- 仅限同主机。
- 跨平台支持不一致(Windows 支持受限)。
- 需要保证路径存在且权限正确。

---

### 4.17 VSOCK

**定位**:虚拟机 ↔ 宿主,或容器沙箱场景。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | `mdlayher/vsock` |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `false` |
| `Caps.MTU` | 65536 |
| 默认端口 | 11010 |

VSOCK 是虚拟机与宿主之间的专用通道,不经过任何网络设备,因此不受 guest 网络配置与防火墙影响。

**配置**

```toml
[transports]
protocols = ["vsock"]

[transports.vsock]
port = 11010
cid  = 2        # 2 = 宿主
```

**优点**

- 不依赖 guest 网络配置,宿主机网络变化不影响。
- 性能接近本地传输。
- 天然隔离,guest 之间互不可见。

**缺点**

- 仅 Linux 与部分虚拟化平台支持。
- 需要宿主与 guest 同时配置。
- 云环境可能不暴露 VSOCK。

---

### 4.18 共享内存

**定位**:同机高频通信,延迟最低。

**实现要点**

| 项 | 值 |
|---|---|
| 库 | 平台相关(mmap / `shm`) |
| `Caps.Reliable` | `true` |
| `Caps.Ordered` | `true` |
| `Caps.Multiplexed` | `true` |
| `Caps.MTU` | 65536 |
| 默认名称 | `/mesh-shm` |

共享内存通过一块 mmap 区域做无锁环形缓冲,做到零拷贝与最低延迟。适合同主机上两个高频通信的 Limesh 实例。

**配置**

```toml
[transports]
protocols = ["shm"]

[transports.shm]
name = "/mesh-shm"
size = "64MB"
```

**优点**

- 延迟最低,零拷贝。
- 不占用端口与文件描述符。

**缺点**

- 仅限同主机。
- 需要一个同步机制(信号量/事件)配合,实现复杂度高。
- 内存占用固定。

---

## 5. 选型决策树

传输选择分两个层面:**自动评分**(默认)与**人工指定**(受限环境)。

### 5.1 自动评分流程

未显式配置 `transports.protocols` 时,按以下顺序逐级判定:

```
                        ┌─────────────────────┐
                        │   对端是否同主机?    │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  本地传输优先      │   │
                  │  共享内存 >       │   │
                  │  Unix Socket >   │   │
                  │  回环 UDP        │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  双方是否都有公网    │
                        │  IPv6 且可达?        │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  IPv6 UDP 直连    │   │
                  │  (无 NAT,成功率   │   │
                  │   最高)           │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  UDP 端口可达?       │
                        │  (STUN 拿到公网候选) │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  QUIC > UDP       │   │
                  │  (按丢包率选择)    │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  能否配置 UPnP /     │
                        │  NAT-PMP / PCP?      │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  端口映射 +       │   │
                  │  UDP 打洞         │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  丢包率是否 > 5%?    │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  KCP (fast 模式)  │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  TCP 端口可达?       │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  TCP 直连         │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  443 是否放行?       │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  WSS > TLS >     │   │
                  │  HTTP/2          │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  22 / 53 / ICMP      │
                        │  是否放行?           │
                        └──────────┬──────────┘
                          是 │           │ 否
                             ▼           │
                  ┌──────────────────┐   │
                  │  SSH > DNS >     │   │
                  │  ICMP (隧道)     │   │
                  └──────────────────┘   │
                                         ▼
                        ┌─────────────────────┐
                        │  经中继转发        │   │
                        │  (Relay 路径)      │   │
                        └─────────────────────┘
```

### 5.2 各节点判定成本

自动评分在探测阶段会**并发**发起多条路径,而不是串行等待。这一点很重要:串行尝试会把最坏情况的建连时间累加成秒级,而并发竞赛的时间代价只取决于最快成功的那条。

```
t=0ms     同时发起:UDP 打洞 / TCP 打洞 / 端口预测 / UPnP 映射
t=150ms   首个成功者胜出,其余取消
t=5000ms  全部失败 → 降级中继(nat.punch_timeout)
```

### 5.3 手动指定

受限环境下显式指定更可靠:

```toml
# 只放行 HTTP/HTTPS 的企业网络
[transports]
protocols = ["wss", "tls", "ws"]
```

按序尝试,首个成功即用。顺序即优先级。

---

## 6. 能力矩阵

### 6.1 核心能力

| 协议 | 可靠 | 有序 | 多路复用 | 自带加密 | 流式 | 建议 MTU | 需要权限 |
|---|---|---|---|---|---|---|---|
| `udp` | ✗ | ✗ | ✗ | ✗ | ✗ | 1380 | 无 |
| `tcp` | ✓ | ✓ | ✗ | ✗ | ✓ | 1380 | 无 |
| `quic` | ✓ | ✓ | ✓ | ✓ | ✓ | 1350 | 无 |
| `tls` | ✓ | ✓ | ✗ | ✓ | ✓ | 1380 | 无 |
| `ws` | ✓ | ✓ | ✗ | ✗ | ✓ | 1380 | 无 |
| `wss` | ✓ | ✓ | ✗ | ✓ | ✓ | 1380 | 无 |
| `http2` | ✓ | ✓ | ✓ | ✓(TLS) | ✓ | 1380 | 无 |
| `http3` | ✓ | ✓ | ✓ | ✓ | ✓ | 1350 | 无 |
| `grpc` | ✓ | ✓ | ✓ | ✓(TLS) | ✓ | 1380 | 无 |
| `kcp` | ✓ | ✓ | ✗ | ✗ | ✓ | 1380 | 无 |
| `dtls` | ✗ | ✗ | ✗ | ✓ | ✗ | 1350 | 无 |
| `wireguard` | ✗ | ✗ | ✗ | ✓ | ✗ | 1420 | 无 * |
| `ssh` | ✓ | ✓ | ✓ | ✓ | ✓ | 1380 | 无 |
| `icmp` | ✗ | ✗ | ✗ | ✗ | ✗ | 1200 | `cap_net_raw` |
| `dns` | ✗/✓ | ✗/✓ | ✗ | ✗ | ✗ | 1200 | 无 |
| `unix` | ✓ | ✓ | ✗ | ✗ | ✓ | 65536 | 无 |
| `vsock` | ✓ | ✓ | ✗ | ✗ | ✓ | 65536 | 无 |
| `shm` | ✓ | ✓ | ✓ | ✗ | ✓ | 65536 | 无 |

\* WireGuard 在内核态实现时需要 `CAP_NET_ADMIN`;用户态实现不需要。

### 6.2 穿透与部署能力

| 协议 | 打洞友好 | 抗封锁 | 需要服务端 | 跨平台 |
|---|---|---|---|---|
| `udp` | 强 | 弱 | 否 | 全平台 |
| `tcp` | 中 | 中 | 否 | 全平台 |
| `quic` | 强 | 中 | 否 | 全平台 |
| `tls` | 弱 | 强 | 是(443) | 全平台 |
| `ws` | 弱 | 中 | 是 | 全平台 |
| `wss` | 弱 | 强 | 是(443) | 全平台 |
| `http2` | 弱 | 强 | 是 | 全平台 |
| `http3` | 中 | 强 | 是 | 全平台 |
| `grpc` | 弱 | 强 | 是 | 全平台 |
| `kcp` | 强 | 中 | 否 | 全平台 |
| `dtls` | 强 | 中 | 否 | 全平台 |
| `wireguard` | 强 | 中 | 是(WG) | Linux 最佳 |
| `ssh` | 弱 | 强 | 是(22) | 全平台 |
| `icmp` | 弱 | 强 | 否 | Linux/macOS |
| `dns` | 弱 | 强 | 是(53) | 全平台 |
| `unix` | — | — | 否 | Linux/macOS |
| `vsock` | — | — | 否 | Linux |
| `shm` | — | — | 否 | Linux/macOS |

### 6.3 数据面行为映射

`Caps` 如何影响数据面封装:

| 场景 | `Reliable=false` | `Ordered=false` | `Multiplexed=true` |
|---|---|---|---|
| 控制消息(LSA/Close) | 应用层确认 + 重传 | — | — |
| 数据包(IP 包) | 不重传(内层协议负责) | 接收端序号重排 | 多 Peer 共享连接 |
| 分片 | 按 `Caps.MTU` 判断 | 分片头参与重排 | 按流独立分片 |

---

## 7. 性能对比

> **测试条件**:同一机房内两台物理机,1 Gbps 链路,虚拟 MTU 1380,ChaCha20-Poly1305 硬件加速(AES-NI + AVX2),单流 iperf3,`mesh` 版本 0.1.0。
>
> **这些是参考量级,不是基准测试结论。** 实际结果随 CPU、网卡、路径 RTT、丢包率与内核参数变化。RTT 与丢包维度通过 `tc netem` 注入。请在目标环境自行验证。

### 7.1 吞吐

| 协议 | 吞吐(零丢包) | 吞吐(5% 丢包) | 相对 UDP |
|---|---|---|---|
| `shm` | ~8 Gbps | ~8 Gbps | 5.0x |
| `unix` | ~7 Gbps | ~7 Gbps | 4.4x |
| `vsock` | ~5 Gbps | ~5 Gbps | 3.1x |
| `udp` | ~1.6 Gbps | ~0.9 Gbps | 1.0x |
| `quic` | ~1.4 Gbps | ~1.1 Gbps | 0.88x |
| `kcp`(fast) | ~1.1 Gbps | ~0.95 Gbps | 0.69x |
| `tcp` | ~1.5 Gbps | ~0.35 Gbps | 0.94x |
| `tls` | ~1.2 Gbps | ~0.3 Gbps | 0.75x |
| `wss` | ~0.9 Gbps | ~0.25 Gbps | 0.56x |
| `http2` | ~1.0 Gbps | ~0.28 Gbps | 0.63x |
| `http3` | ~1.2 Gbps | ~0.9 Gbps | 0.75x |
| `grpc` | ~0.8 Gbps | ~0.22 Gbps | 0.5x |
| `ssh` | ~0.3 Gbps | ~0.15 Gbps | 0.19x |
| `icmp` | ~5 Mbps | ~3 Mbps | 0.003x |
| `dns` | ~2 Mbps | ~1 Mbps | 0.001x |

关键观察:

- **丢包是分水岭**。零丢包时 TCP 系与 UDP 系差距不大;5% 丢包时 TCP 掉到 0.35 Gbps,而 QUIC 仍有 1.1 Gbps。这就是"按丢包率选传输"的依据。
- **本地传输与网络传输差一个数量级**,同机对端应优先本地传输。
- **隧道类协议(SSH/ICMP/DNS)性能代价极大**,是可用性兜底而非性能选项。

### 7.2 延迟

| 协议 | 单跳延迟增量 | 建连时间 | 说明 |
|---|---|---|---|
| `shm` | ~0.01 ms | <1 ms | 零拷贝,无握手 |
| `unix` | ~0.05 ms | <1 ms | 无网络栈 |
| `vsock` | ~0.1 ms | <1 ms | 虚拟化通道 |
| `udp` | +0.3 ms | 150 ms(含握手) | 基准 |
| `kcp`(fast) | +0.5 ms | 180 ms | 激进重传换低延迟 |
| `quic` | +0.6 ms | 120 ms(0-RTT 时更低) | 无队头阻塞 |
| `tcp` | +0.7 ms | 300 ms | 握手 + 队头阻塞 |
| `dtls` | +0.8 ms | 250 ms | 数据报 TLS |
| `tls` | +0.9 ms | 350 ms | 双层加密 |
| `wireguard` | +0.4 ms | 200 ms | 性能优秀 |
| `wss` | +1.1 ms | 400 ms | WebSocket 帧开销 |
| `http2` | +1.0 ms | 380 ms | 头压缩 |
| `http3` | +0.7 ms | 150 ms | 继承 QUIC |
| `grpc` | +1.2 ms | 400 ms | 流控开销 |
| `ssh` | +1.5 ms | 500 ms | 加密层数多 |
| `icmp` | +5 ms | 800 ms | 受 ping 频率限制 |
| `dns` | +10 ms | 1200 ms | 受查询频率与报文大小限制 |

### 7.3 带宽放大

带宽放大 = 实际链路流量 / 有效载荷流量。比值越高,对链路压力越大。

| 协议 | 零丢包 | 5% 丢包 | 20% 丢包 |
|---|---|---|---|
| `udp` | 1.03 | 1.05 | 1.1 |
| `quic` | 1.05 | 1.12 | 1.25 |
| `kcp`(fast) | 1.1 | **1.35** | **1.7** |
| `kcp`(normal) | 1.06 | 1.2 | 1.4 |
| `tcp` | 1.04 | 1.15 | 1.35 |
| `shm` / `unix` / `vsock` | 1.0 | 1.0 | 1.0 |

KCP 的 `fast` 模式用带宽换延迟,高丢包下放大到 1.7 倍。在按流量计费的链路上需要权衡。

### 7.4 CPU 开销

相对 `udp` 的单核 CPU 占用(转发 1 Gbps 流量):

| 协议 | CPU 相对开销 | 主要来源 |
|---|---|---|
| `udp` | 1.0x | 加解密 |
| `shm` / `unix` | 0.6x | 无网络栈 |
| `quic` | 1.4x | 加解密 + 流控 |
| `kcp` | 1.5x | 重传 + 队列管理 |
| `tcp` | 1.3x | 内核栈 + 拷贝 |
| `tls` / `wss` | 2.0x | 双层加密 |
| `http2` / `grpc` | 2.2x | 帧处理 + 流控 |
| `ssh` | 2.5x | 多层加密 |
| `icmp` / `dns` | 3.0x | 逐包封装开销 |

---

## 8. 扩展指南

新增一种传输只需实现 `Transport` 接口并注册,不需要改动路由、加密或 Peer 状态机。

### 8.1 实现步骤

**第 1 步 · 创建包**

```
internal/transport/mine/
└── mine.go
```

**第 2 步 · 实现接口**

```go
package mine

import (
    "context"

    "github.com/LiStudioorg/limesh/internal/transport"
)

// Transport 是 mine 传输的实现。
type Transport struct {
    cfg transport.TransportConfig
}

// New 构造一个 mine 传输实例。
func New(cfg transport.TransportConfig) *Transport {
    return &Transport{cfg: cfg}
}

// Name 返回注册名,与配置中的 transports.protocols 项一一对应。
func (t *Transport) Name() string { return "mine" }

// Dial 主动连接到指定对端。
func (t *Transport) Dial(ctx context.Context, peer transport.PeerAddr) (transport.Conn, error) {
    // 建立连接,返回实现 transport.Conn 的对象。
    return nil, nil
}

// Listen 监听入站连接。
func (t *Transport) Listen(ctx context.Context, addr string) (transport.Listener, error) {
    return nil, nil
}

// Capabilities 声明该传输的能力。
// 必须如实声明:数据面完全依赖这些字段决定是否叠加重传、重排与分片。
func (t *Transport) Capabilities() transport.Caps {
    return transport.Caps{
        Reliable:    false,
        Ordered:     false,
        Multiplexed: false,
        MTU:         1380,
    }
}
```

**第 3 步 · 注册**

```go
func init() {
    transport.Register("mine", func(cfg transport.TransportConfig) (transport.Transport, error) {
        return New(cfg), nil
    })
}
```

**第 4 步 · 接入注册表**

在 `internal/transport/all/all.go` 中导入新包(仅为此包的副作用):

```go
package all

import (
    _ "github.com/LiStudioorg/limesh/internal/transport/mine"
    _ "github.com/LiStudioorg/limesh/internal/transport/udp"
    // ...
)
```

**第 5 步 · 配置结构**

在 `internal/transport/schema.go` 中为 `[transports.mine]` 定义配置结构,并把字段接入配置合并。

**第 6 步 · 测试**

```go
func TestMineEcho(t *testing.T) {
    // 起监听 → Dial → 双向写读 → 校验字节一致
}

func TestMineCapsHonest(t *testing.T) {
    // 断言 Capabilities() 与实际行为一致:
    //   Reliable=true  → 丢包后不丢数据
    //   Ordered=true   → 乱序注入后仍按序读出
    //   Multiplexed=true → 两个 Conn 共享一次底层连接
}
```

### 8.2 实现要点

| 要点 | 说明 |
|---|---|
| `Caps` 必须如实 | 虚报 `Reliable=true` 会让数据面关闭重传,导致控制消息丢失 |
| `Read` 的切片生命周期 | 返回的切片在下次 `Read` 前有效,调用方不得长期持有 |
| `Write` 不得引用入参 | 实现必须在返回前复制或发送完毕 |
| 上下文取消 | `Dial` / `Accept` 必须响应 `ctx.Done()` |
| 资源释放 | `Close` 必须释放 socket、goroutine 与缓冲 |
| 地址收集 | `LocalAddr` / `RemoteAddr` 是候选地址的来源,必须如实返回 |
| 统计 | `Stats()` 的 `RTT` / `Loss` 是路由打分的输入,必须真实采样 |
| 无 CGO | 不得引入 C 依赖,否则破坏交叉编译 |

### 8.3 常见错误

| 错误 | 后果 |
|---|---|
| `Caps.Reliable` 虚报为 `true` | 控制消息丢失,路由收敛异常 |
| `Caps.Ordered` 虚报为 `true` | 上层不做重排,IP 包乱序 |
| 忽略 `ctx` 取消 | 连接泄漏,goroutine 堆积 |
| `Write` 直接引用调用方切片 | 数据竞争,内容错乱 |
| `Stats()` 返回固定值 | 路由打分失效,`latency` 策略退化 |
| 引入 CGO 依赖 | 交叉编译失败,违反项目铁律 |

---

## 9. 常见问题

### 9.1 该选哪种传输?

**默认不要选。** 留空 `transports.protocols` 让自动评分处理,它会按[决策树](#5-选型决策树)逐级判定。

只有两种情况下需要手动指定:

1. **已知网络限制**:企业网络只放行 443,就配 `["wss", "tls"]`。
2. **需要强制某条路径**:例如强制所有流量走 KCP 以规避 QoS。

### 9.2 多个协议同时配置会怎样?

按配置顺序**依次尝试**,首个成功即用:

```toml
[transports]
protocols = ["udp", "quic", "tcp"]
```

先试 UDP;失败试 QUIC;再失败试 TCP。全部失败则降级中继。

顺序即优先级,把期望的协议放前面。

### 9.3 隧道传输为什么这么慢?

因为它在做"封装 + 伪装",每一步都有代价:

```
应用数据
  → ZCPacket 封装
  → ChaCha20-Poly1305 加密         ← 一层加密
  → 外层协议封装(SSH/DNS/ICMP)     ← 封装开销
  → 外层协议加密(SSH)              ← 又一层加密
  → 传输
```

ICMP 与 DNS 还额外受频率限制:ICMP 受系统速率限制,DNS 受报文大小与查询频率限制,吞吐天花板在 Mbps 量级。

**结论:隧道传输是可用性兜底,不是性能选项。** 只在其他协议全部不可用时启用。

### 9.4 UDP 被 QoS 了怎么办?

常见表现:能连通但吞吐远低于带宽上限,或连接频繁抖动。处理顺序:

```toml
# 1. 换 KCP(仍是 UDP,但重传策略不同,部分 QoS 策略不识别)
[transports]
protocols = ["kcp"]

[transports.kcp]
mode = "normal"     # fast 模式流量特征更明显

# 2. 换 QUIC(加密后特征不同)
[transports]
protocols = ["quic"]

# 3. 换 TCP 系
[transports]
protocols = ["tcp", "tls"]
```

### 9.5 双方配置的协议不一致能连通吗?

**能,只要有交集。** 双方各自发起探测,取双方都支持的协议。

```
节点 A: ["udp", "quic", "tcp"]
节点 B: ["tcp", "tls"]
交集:   ["tcp"]              ← 用这个建连
```

无交集时无法建连,但会降级到中继路径。

### 9.6 WireGuard 传输的 MTU 为什么不一样?

因为存在两层封装:

```
原有:  IP 包(1380) + ZCPacket 头(48) + UDP/IP ≈ 1450
WG:    IP 包(1380) + ZCPacket 头(48) + WG 头(32) + UDP/IP ≈ 1482   ← 超过 1500
```

`Caps.MTU` 标为 1420 是为了给 WG 头留出空间,避免路径 MTU 溢出导致分片。实际部署时若发现大量分片,可进一步下调 `network.mtu`。

### 9.7 本地传输为什么不是默认?

因为它需要双方**在同一台机器上**,而这是少数情况。自动评分已经处理了:`prefer_local = true`(默认)时,同机对端会自动优先本地传输。

验证是否走了本地传输:

```bash
mesh peer show <node-id> | grep Transport
```

```
Transport    shm (unix available)
```

### 9.8 如何确认当前用的是哪种传输?

```bash
mesh peer list
```

```
NODE ID            HOSTNAME   VIRTUAL IP       PATH     RTT      LOSS   TRANSPORT
5c1d8f2ab9034e77   node-c     10.126.126.87    direct   8.4ms    0.0%   udp
a1b2c3d4e5f60718   relay-a    —                relayed  12.1ms   0.0%   quic
```

```bash
mesh peer show 5c1d8f2ab9034e77
```

```
NodeID       5c1d8f2ab9034e77
Path         direct
Endpoint     203.0.113.9:44321
Transport    udp (quic available, kcp available)
RTT          8.4ms   (ewma 8.6ms)
Loss         0.0%
```

括号内是**可用但未启用**的候选,可用于判断是否有更优路径可切换。

### 9.9 传输频繁切换会导致什么?

频繁切换会造成 IP 包乱序,进而触发内层 TCP 的重传。这正是路由层需要 EWMA 平滑的原因:

```toml
[routing]
ewma_alpha = 0.2    # 越小越平滑
```

如果观察到连接频繁抖动,调小 `ewma_alpha` 并检查 `routing.strategy` 是否设成了过于敏感的 `latency`。

### 9.10 如何新增一种传输而不破坏现有行为?

三条纪律:

1. **如实声明 `Caps`** —— 数据面完全信任这个声明,虚报会导致静默错误。
2. **不改核心代码** —— 只在 `internal/transport/<name>/` 下新增包;若发现必须改核心,说明抽象有缺口,应先讨论接口设计。
3. **补能力测试** —— 断言 `Caps` 声明与实际行为一致(见[扩展指南](#8-扩展指南)第 6 步)。

---

**文档版本**:v1.0
**对应总纲**:[AGENTS.md](../AGENTS.md) 第 9 章
**相关文档**:[CONFIG.md](CONFIG.md) · [README.md](../README.md)
**许可证**:Apache 2.0
