# Limesh 技术方案

> Lightweight mesh. Instantly.
>
> 本文档面向 Limesh 的开发者与贡献者,描述系统的设计目标、分层结构、协议规范、配置系统与工程约束。用户文档见 [README.md](</README.md>)。

---

## 目录

1. [项目概述](#1-项目概述)
2. [命名体系](#2-命名体系)
3. [核心设计原则](#3-核心设计原则)
4. [功能全集](#4-功能全集)
5. [架构分层](#5-架构分层)
6. [代码结构](#6-代码结构)
7. [技术选型](#7-技术选型)
8. [通信协议](#8-通信协议)
9. [传输协议](#9-传输协议18-种)
10. [配置系统](#10-配置系统)
11. [单二进制角色](#11-单二进制角色)
12. [三种 Ingress 模式](#12-三种-ingress-模式)
13. [权限模型](#13-权限模型)
14. [平台支持](#14-平台支持)
15. [Docker 部署](#15-docker-部署)
16. [K8s 部署](#16-k8s-部署)
17. [许可证与合规](#17-许可证与合规)
18. [路线图](#18-路线图)
19. [构建与发布](#19-构建与发布)
20. [风险与对策](#20-风险与对策)

---

## 1. 项目概述

Limesh(立网)是一个**去中心化、多协议、智能路由、可观测、可运营**的组网平台。它把若干台分散在家庭、云、机房、容器里的机器,虚拟成一个扁平的三层网络:每台机器获得一个稳定的虚拟 IP,彼此可以直接发 IP 包,不关心底层是 NAT、防火墙、还是跨洲链路。

对标产品:

| 产品 | Limesh 的差异点 |
|---|---|
| EasyTier | 同为 P2P 组网,但 Limesh 提供 18 种传输协议插件、可插拔数据面与四层配置合并 |
| Tailscale | 同为 WireGuard 系,但 Limesh 不绑定单一控制面,可自带 controller 自托管,且提供零权限中继 |
| NetBird | 同样强调 ACL 与可观测,但 Limesh 允许无 root 的 gVisor 用户态栈作为一等公民 |
| ZeroTier | 同样有 L2 桥接与虚拟交换机语义,但 Limesh 强制端到端加密且中继不可解密 |

一句话:**一个二进制,把"能连上的机器"变成"在同一个局域网里"。**

设计上追求三件事:

1. **接入无门槛** —— 一条命令起节点,无 root 也能跑。
2. **路径最优** —— 能直连就直连,不能直连才中继,中继永远不是瓶颈的唯一解。
3. **可运营** —— 拓扑、指标、审计日志都是一等输出,而非事后补丁。

---

## 2. 命名体系

命名在项目内是**契约**,不允许各模块自行发明别名。所有对外产物统一如下:

| 项 | 值 |
|---|---|
| 产品名 | Limesh |
| 中文名 | 立网 |
| 二进制 | `mesh` |
| 守护进程 | `meshd` |
| 配置文件 | `/etc/mesh/config.toml` |
| 状态目录 | `/var/lib/mesh/` |
| 镜像 | `limesh/mesh` |
| 环境变量 | `MESH_*` |
| 默认网段 | `10.126.126.0/24` |
| 默认端口 | `11010` |
| 标语 | Lightweight mesh. Instantly. |
| 官网 | limesh.dev |
| 仓库 | https://github.com/LiStudioorg/limesh |

约定:

- 网段 `10.126.126.0/24` 中,`10.126.126.1` 保留给控制面/网关角色。
- 端口 `11010` 同时用于 UDP 与 TCP 监听;`11010/udp` 是主数据面端口。
- 环境变量一律 `MESH_` 前缀,例如 `MESH_NETWORK_SECRET`、`MESH_LOG`。
- 文档中"节点"指运行 `mesh node` 的进程实例,"对端"指同网络中的另一个节点。

---

## 3. 核心设计原则

八条原则是评审代码时的**否决依据**:违反其中任何一条的 PR 不予合入。

### 3.1 单二进制

客户端、中继、控制面、网关、出口节点全部编译进同一个 `mesh` 可执行文件。理由:

- 分发成本最低:一条 `curl | sh`,一个 Docker 镜像,一个 K8s DaemonSet。
- 版本一致性天然成立:不存在"relay 是新版、node 是旧版"的兼容矩阵地狱。
- 攻击面可控:未启用的角色其代码路径不会被激活,由 cobra 子命令在入口处裁剪。

实现方式:每个角色实现 `Role` 接口,`main` 根据子命令只构造并启动一个 Role。角色之间通过接口通信,不允许直接引用彼此的内部包。

### 3.2 零 CLI 依赖的 core

核心库(`internal/` 与 `pkg/`)不得 import `cobra`、`pflag`,不得读取 `os.Args`。CLI 只是 core 的一个适配器。

理由:移动端(gomobile)无法使用命令行解析;把参数解析泄漏进 core 会导致 Android/iOS 桥接层被迫携带一整套 CLI 语义。所有配置必须经由结构体注入。

### 3.3 数据面与接入层解耦

接入层(TUN / gVisor 用户态栈 / SOCKS5 / 端口转发)只负责**把 IP 包交给数据面**与**把 IP 包投递给内核或应用**。数据面(ZCPacket、加密、多路径)不感知自己上面挂的是 TUN 还是 netstack。

结果:无 root 时把 TUN 换成 gVisor 只是替换一个 `Ingress` 实现,其余全部复用。

### 3.4 传输可插拔

18 种传输协议实现同一个接口。任何新增协议只需注册一个工厂函数,不改动路由、加密、Peer 状态机。

### 3.5 配置单一真相源

CLI 参数、环境变量、TOML 文件、内嵌 `[cli]` 块、内置默认值,最终合并为**一个不可变的 `Config` 快照**。运行期所有模块只读该快照,不允许各自读取环境变量。

### 3.6 热重载可预测

每个模块必须声明自己对配置变更的响应级别:

| 级别 | 语义 | 例子 |
|---|---|---|
| `hot` | 原地生效,不中断连接 | 日志级别、路由策略权重 |
| `reconnect` | 需要重建传输,虚拟 IP 与路由保留 | 监听端口、传输协议列表 |
| `restart` | 必须重启进程 | 网络密钥、NodeID 相关项 |

热重载逐模块 `Apply`,任一模块失败即**回滚到上一份快照**,不允许部分生效。

### 3.7 纯 Go 优先

`CGO_ENABLED=0` 是硬性要求。所有依赖必须是纯 Go 且许可证与 Apache 2.0 兼容。

理由:交叉编译 30+ 平台、静态链接、scratch 镜像、无 libc 依赖的 Alpine 部署,全部依赖这一条。

### 3.8 权限按需

中继零权限即可运行;节点按需提权;移动端由系统能力豁免。核心目标:**尽可能让"跑得起来"不需要 root**。无 root 时自动降级到 no-tun 模式,而不是直接失败退出。

---

## 4. 功能全集

### 4.1 组网核心

| 能力 | 说明 |
|---|---|
| P2P 直连 | 打洞成功后节点间直接收发 UDP/TCP,不经过第三方 |
| 中继 | 打洞失败时由 relay 转发,保持 E2EE,中继看不到明文 |
| OSPF 路由 | 网络内自动收敛路由,支持多跳与冗余路径 |
| 子网代理 | 把节点背后的物理子网(如 `192.168.1.0/24`)宣告进虚拟网络 |
| 出口节点 | 所有默认流量经指定节点出公网 |
| 入口节点 | 接收来自虚拟网络外部流量的入口 |
| L2 桥接 | 同网段二层帧转发,支持非 IP 协议 |
| 广播/多播 | 组播与广播在虚拟网络内转发 |

### 4.2 NAT 穿透

STUN、ICE、UPnP、NAT-PMP、PCP、UDP 打洞、TCP 打洞、端口预测、生日悖论、IPv6 优先。

策略:**并发竞赛**,所有可用策略同时发起,首个建立直连的胜出,其余取消。不做串行尝试,因为串行会把最坏情况下的建连时间累加成秒级。

### 4.3 传输协议

共 18 种,详见 [第 9 章](#9-传输协议18-种):UDP、TCP、QUIC、TLS、WS、WSS、HTTP/2、HTTP/3、gRPC、KCP、DTLS、WireGuard、SSH、ICMP、DNS、Unix Socket、VSOCK、共享内存。

### 4.4 安全

- **E2EE**:Noise IK 握手,ChaCha20-Poly1305 加密,前向保密。
- **细粒度 ACL**:按节点、子网、端口、协议维度允许/拒绝。
- **密钥轮换**:网络密钥与节点密钥均可在线轮换,旧密钥有重叠窗口。
- **审计日志**:所有控制面操作与 ACL 拒绝事件结构化落盘。

### 4.5 路由策略

`shortest`(跳数最短)、`latency`(延迟最低)、`weighted`(加权)。EWMA 平滑以避免路径抖动;支持多路径并行;中继选择优先地理就近。

### 4.6 接入模式

TUN(需 root)、No-TUN(gVisor 用户态栈,无需 root)、Auto(检测失败自动降级)。

### 4.7 可观测

Prometheus 指标端点、拓扑 API(JSON)、结构化日志(zap)。

### 4.8 配置

TOML 单格式、四层优先级、热重载、原子替换、通配符、include、内嵌 `[cli]` 块。

### 4.9 部署

单二进制、Docker 多架构、Compose、K8s、一键脚本。

### 4.10 移动端(后期)

gomobile + Android VPNService / iOS NetworkExtension。

---

## 5. 架构分层

系统按职责分为 8 层。分层是**依赖方向约束**:上层可依赖下层,下层不得反向依赖上层(通过接口注入解耦)。

```
┌─────────────────────────────────────────────────────────────┐
│ L7 应用层   CLI | Web 面板 | API | 移动端桥接                │
├─────────────────────────────────────────────────────────────┤
│ L6 控制面   配置管理 | 热重载 | 拓扑同步 | 密钥管理          │
├─────────────────────────────────────────────────────────────┤
│ L5 路由层   OSPF | 策略引擎 | 路径选择 | Peer 状态机         │
├─────────────────────────────────────────────────────────────┤
│ L4 数据面   ZCPacket | 多路径 | QoS | 压缩 | 加密            │
├─────────────────────────────────────────────────────────────┤
│ L3 传输层   18 种传输协议                                    │
├─────────────────────────────────────────────────────────────┤
│ L2 穿透层   STUN | ICE | UPnP | NAT-PMP | 端口预测 | 生日悖论│
├─────────────────────────────────────────────────────────────┤
│ L1 接入层   TUN | gVisor Netstack | SOCKS5 | 端口转发        │
├─────────────────────────────────────────────────────────────┤
│ L0 观测层   Prometheus | 日志 | 审计                         │
└─────────────────────────────────────────────────────────────┘
```

### 5.1 各层职责

| 层 | 职责 | 不做什么 |
|---|---|---|
| L7 | 参数解析、渲染输出、REST/gRPC 暴露、移动端 API | 不碰 socket,不做路由决策 |
| L6 | 配置合并与热重载、拓扑广播、密钥生命周期 | 不直接收发数据包 |
| L5 | 路由计算、路径打分、Peer 建连状态推进 | 不关心承载协议细节 |
| L4 | 封装/解封装、加密解密、分片与重组、多路径调度 | 不做路由决策 |
| L3 | 字节流/数据报承载,可靠性由协议自身决定 | 不解析 IP 包 |
| L2 | NAT 类型探测、打洞协调、端口映射 | 不参与数据转发 |
| L1 | 从内核/应用取包,向内核/应用投包 | 不加密、不路由 |
| L0 | 采集与导出,只读旁路 | 不介入主链路(采样失败不得阻断转发) |

### 5.2 关键数据流(节点收到一个虚拟 IP 包)

```
内核 → TUN 读 → L1 Ingress
     → L4 查路由表得下一跳
     → L4 ZCPacket 封装 + ChaCha20-Poly1305 加密
     → L3 选择该 Peer 的活跃传输
     → 网络 → 对端 L3 → L4 解密 + 校验 NodeID
     → L1 Ingress 写入对端 TUN → 内核
```

中继路径下,L3 到对端之间多一跳 relay;relay 只看到 ZCPacket 的外层寻址字段,**payload 保持密文**。

---

## 6. 代码结构

```
limesh/
├── cmd/
│   └── mesh/
│       └── main.go              # 唯一入口:解析子命令 → 构造 Role → 启动
├── internal/
│   ├── cli/                     # cobra 命令树(唯一允许 import cobra 的地方)
│   │   ├── root.go
│   │   ├── node.go
│   │   ├── relay.go
│   │   ├── controller.go
│   │   ├── gateway.go
│   │   ├── exit.go
│   │   ├── config.go
│   │   ├── peer.go
│   │   ├── route.go
│   │   ├── topo.go
│   │   └── status.go
│   ├── config/                  # L6 配置系统
│   │   ├── schema.go            # Config 结构体(单一真相源)
│   │   ├── merge.go             # 四层合并
│   │   ├── include.go           # include + 通配符展开
│   │   ├── sources.go           # 来源追踪
│   │   ├── watch.go             # fsnotify + SIGHUP
│   │   └── apply.go             # 逐模块 Apply + 回滚
│   ├── packet/                  # L4 ZCPacket 编解码
│   │   ├── packet.go
│   │   ├── types.go
│   │   └── codec_test.go
│   ├── crypto/                  # Noise IK + AEAD
│   ├── transport/               # L3 传输注册表
│   │   ├── transport.go         # Transport 接口 + Registry
│   │   ├── udp/
│   │   ├── tcp/
│   │   ├── quic/
│   │   ├── kcp/
│   │   └── ...
│   ├── punch/                   # L2 穿透
│   │   ├── stun/
│   │   ├── ice/
│   │   ├── upnp/
│   │   └── predict/
│   ├── route/                   # L5 OSPF + 策略引擎
│   ├── peer/                    # L5 Peer 状态机
│   ├── ingress/                 # L1 接入层
│   │   ├── ingress.go           # Ingress 接口
│   │   ├── tun/
│   │   ├── netstack/
│   │   ├── socks5/
│   │   └── forward/
│   ├── role/                    # 单二进制角色实现
│   │   ├── node/
│   │   ├── relay/
│   │   ├── controller/
│   │   ├── gateway/
│   │   └── exit/
│   ├── obs/                     # L0 观测
│   │   ├── metrics/
│   │   ├── logging/
│   │   └── audit/
│   └── acl/                     # 访问控制
├── pkg/
│   └── mesh/                    # 对外稳定 API(供移动端与第三方嵌入)
├── docs/
│   ├── CONFIG.md
│   └── TRANSPORT.md
├── AGENTS.md                    # 本文件:内部技术方案
├── README.md                    # 用户门面文档
├── deploy/
│   ├── docker/
│   ├── k8s/
│   └── systemd/
├── scripts/
│   └── install.sh
├── Makefile
└── go.mod
```

### 6.1 包依赖规则

| 规则 | 说明 |
|---|---|
| `internal/cli` 可依赖一切 | 它是适配器层 |
| `pkg/mesh` 只依赖 `internal/*` 中稳定的接口包 | 不暴露内部实现 |
| `internal/role/*` 之间不可互相 import | 通过接口组合 |
| `internal/transport/*` 不可 import `internal/route` | 传输不感知路由 |
| `internal/config` 不 import 任何业务包 | 只描述数据结构 |
| 任何包不得 import `internal/cli` | 强制单向依赖 |

CI 中用 `go list -deps` 做依赖方向断言,违反即失败。

---

## 7. 技术选型

| 领域 | 选型 | 理由 |
|---|---|---|
| 语言 | Go 1.22+ | 交叉编译、goroutine 模型、生态成熟 |
| 配置 | koanf + TOML | 原生多源合并与来源追踪,天然匹配四层优先级 |
| CLI | cobra + pflag | 子命令树与 POSIX flag 兼容性最好 |
| TUN | wireguard-go/tun | 跨平台 TUN 抽象成熟,已被大规模验证 |
| 用户态栈 | gvisor netstack | 纯 Go、性能可接受、无需 root |
| 加密 | WireGuard Noise | Noise IK 有完整 Go 实现,前向保密与抗重放开箱即用 |
| QUIC | quic-go | 纯 Go,HTTP/3 与原生 QUIC 共用 |
| KCP | xtaci/kcp-go | 弱网下优于 TCP 的可靠 UDP |
| STUN/ICE | pion/stun + pion/ice | 纯 Go,ICE 状态机完整 |
| 日志 | zap | 结构化、低分配 |
| 指标 | prometheus/client_golang | 事实标准 |

### 7.1 铁律

1. `CGO_ENABLED=0`,任何引入 C 依赖的 PR 一律拒绝。
2. 所有依赖必须纯 Go。
3. 所有依赖许可证必须与 Apache 2.0 兼容(GPL/AGPL 系依赖禁止引入)。
4. 新增依赖需在 PR 描述中说明:替代方案、体积影响、维护活跃度。

---

## 8. 通信协议

### 8.1 协议栈

```
┌──────────────────────────────────────────┐
│ 应用数据(IP 包)                          │
├──────────────────────────────────────────┤
│ ZCPacket 头(48 字节固定)                 │
├──────────────────────────────────────────┤
│ ChaCha20-Poly1305 AEAD(Noise IK 派生密钥)│
├──────────────────────────────────────────┤
│ 传输层(UDP / QUIC / TCP / WS / ...)      │
├──────────────────────────────────────────┤
│ IP / NAT / 物理链路                       │
└──────────────────────────────────────────┘
```

设计取值说明:

- **固定 48 字节头**:无变长字段意味着可以零拷贝解析,且长度可预测,便于 `sync.Pool` 复用缓冲。
- **AEAD 覆盖整个 payload**:头部的寻址字段(SrcNodeID/DstNodeID)参与 AAD 校验,防止中间人改写成"投递给别人"。
- **SessionID + Sequence + Ack** 组合承担重放窗口与可靠控制消息的确认职责,避免在上层再造一套。

### 8.2 身份与标识

| 标识 | 推导方式 | 用途 |
|---|---|---|
| 节点密钥对 | 本地生成,Ed25519/X25519 | Noise IK 静态密钥 |
| `NodeID` | `SHA256(pubkey)[:16]`(16 字节) | 全网唯一身份,LSA 与路由的键 |
| `VirtualIP` | 由 `NodeID` 确定性派生 | 虚拟网络内寻址 |
| `SessionID` | 每次握手协商产生(4 字节) | 一次会话的多路径复用与重放防护 |

要点:

- `NodeID` 是**身份**而非地址。改 IP、换网卡、换中继都不改变 `NodeID`。
- `VirtualIP` 由 `NodeID` 派生意味着任何节点都能在无中心协调的情况下,独立算出对端的虚拟 IP:不需要 DHCP,不存在分配冲突。
- `VirtualIP` 派生需避开网络地址与广播地址,并跳过 `10.126.126.1` 保留位。

### 8.3 ZCPacket 线格式

固定 48 字节头 + 变长 payload,所有多字节字段采用**网络字节序(大端)**。

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    Magic      |     Ver       |     Type      |     Flags     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                         SessionID                             |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          Sequence                             |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                            Ack                                |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
+                     SrcNodeID (16 bytes)                      +
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
+                     DstNodeID (16 bytes)                      +
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          Payload ...                          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

| 字段 | 长度 | 说明 |
|---|---|---|
| `Magic` | 1 | 固定魔数,用于快速丢弃非法包 |
| `Ver` | 1 | 协议版本,便于灰度升级 |
| `Type` | 1 | 包类型,见 8.4 |
| `Flags` | 1 | 位标志:加密、中继、探测、分片首/末 |
| `SessionID` | 4 | 会话标识 |
| `Sequence` | 4 | 单调递增序号 |
| `Ack` | 4 | 累计确认序号 |
| `SrcNodeID` | 16 | 源节点身份 |
| `DstNodeID` | 16 | 目的节点身份 |
| `Payload` | 变长 | 密文(或控制消息体) |

头部合计 `1+1+1+1+4+4+4+16+16 = 48` 字节。

### 8.4 包类型

| 类型 | 名称 | 承载 | 可靠性 |
|---|---|---|---|
| `TypeHandshake` | 握手 | Noise IK 消息 | 重传至超时 |
| `TypeHandshakeResp` | 握手响应 | Noise IK 响应 | 重传至超时 |
| `TypeData` | 数据 | 加密 IP 包 | 尽力而为(由内层协议决定) |
| `TypeKeepalive` | 保活 | 空体 | 无需确认 |
| `TypeLSA` | 路由通告 | 链路状态 | 需要确认 |
| `TypePunch` | 打洞 | 探测载荷 | 无需确认 |
| `TypeRelay` | 中继 | 转发的 ZCPacket | 尽力而为 |
| `TypeProbe` | 探测 | 时间戳 | 无需确认 |
| `TypeClose` | 关闭 | 原因码 | 需要确认 |

设计取舍:

- **数据包不重传**。虚拟网络承载的是 IP 包,TCP 的重传由内层 TCP 负责;UDP 本就允许丢包。在数据面加可靠性会造成"可靠之上再可靠"的双重重传,既浪费带宽又增加尾延迟。
- **控制包显式确认**。LSA 与 Close 必须可靠,否则路由收敛会产生黑洞。

### 8.5 Noise IK 握手

采用 Noise IK 模式,**1-RTT** 完成双向认证与密钥派生,无需预先知道对端地址之外的信息。

```
发起方 A                                          响应方 B
   │                                                 │
   │ 预先知道 B 的静态公钥(s)                        │
   │                                                 │
   │── e ─────────────────────────────────────────▶  │
   │── es, s, ss(加密的 A 静态公钥) ──────────────▶  │   A 的身份在此暴露
   │                                                 │
   │  ◀─────────────────────── e, ee, se ────────────│
   │                                                 │
   │ 双方派生:发送密钥 / 接收密钥 / SessionID         │
   │═══════════ 加密通道建立(1-RTT)═══════════════│
```

安全属性:

| 属性 | 保障方式 |
|---|---|
| 前向保密 | 每会话临时密钥对 `e`,会话结束后临时私钥销毁 |
| 双向认证 | IK 模式中发起方已知响应方公钥;响应方通过密文获得发起方公钥 |
| 抗重放 | `Sequence` 单调递增 + 滑动接收窗口,窗口外直接丢弃 |
| 抗 DoS | 握手第一阶段不进行昂贵运算;对单 IP 的握手尝试限速;响应方在验证发起方 token 前不分配会话状态 |
| 中继不可解密 | 中继转发的是密文 ZCPacket,密钥仅两端持有 |

握手失败处理:指数退避重试,退避上限后转入中继协商,而不是无限重试直连。

### 8.6 控制协议

#### 8.6.1 Keepalive

- 周期:**20 秒**。
- 目的:维持 NAT 映射、探测 Peer 存活、为 EWMA 提供延迟样本。
- 连续 3 个周期(60 秒)无任何包 → 标记 Peer 为 `Degraded`;连续 6 个周期 → `Dead`,触发路由重新收敛。

#### 8.6.2 LSA(OSPF 子集)

节点把自己的直连邻居与链路开销封装为 LSA 泛洪到全网。

| 字段 | 说明 |
|---|---|
| Originator | 发起节点 `NodeID` |
| Sequence | LSA 序号,用于判断新旧 |
| Age | 存活时间,超时失效 |
| Links | 邻居列表 + 链路开销(延迟 EWMA) |

收敛:收到 LSA 后在本地链路状态数据库执行 Dijkstra,得到到所有节点的最短路径树,再展开为转发表。

#### 8.6.3 Punch(打洞)

- 双方通过已有信道(中继或控制面)交换候选地址集合。
- 所有策略**并发**发起:UDP 打洞、TCP 打洞、端口预测、生日悖论、UPnP/NAT-PMP/PCP 端口映射。
- 首个成功的候选立即胜出并广播 `Punch` 成功包,其余策略取消。
- 打洞包带有随机载荷,防止被中间设备误判为扫描流量。

#### 8.6.4 Relay(中继)

- 打洞超时(默认 5 秒)后进入中继路径。
- 中继只依据 `DstNodeID` 查表转发,**不解析 payload**。
- 中继保持 E2EE:两端密钥未经过中继。
- 中继定期尝试重新打洞(默认 60 秒一次),一旦直连成功即切换回直连路径。

#### 8.6.5 Probe(探测)

- 用于链路质量采样:发送带时间戳的探测包,接收方原样回显。
- 采样结果喂给 EWMA,作为 `latency` 路由策略的输入。
- 采样频率自适应:链路稳定时降频,抖动时升频。

### 8.7 数据协议

数据包 = `TypeData` 的 ZCPacket,payload 为 **ChaCha20-Poly1305 加密的 IP 包**。

- **Nonce 构造**:由 `SessionID` 与 `Sequence` 组合,保证同一会话内不重用。
- **AAD**:ZCPacket 头的寻址与类型字段,防止头部被篡改。
- **MTU**:虚拟 MTU 默认 `1380`,为 ZCPacket 头(48)与传输层封装预留空间,避免分片。
- **分片**:payload 超过传输层可承载长度时按 `Flags` 中的分片位切分,接收端重组;重组有超时,超时丢弃整组。
- **多路径**:同一 Peer 的多个可用传输被并行使用,包按序号轮转发送,接收端按序号统一去重与排序,避免单路径抖动导致整体卡顿。

### 8.8 路由协议(OSPF 子集)

Limesh 采用 OSPF 的**链路状态思想**,但只实现组网必需的子集。

| 保留 | 简化/省略 |
|---|---|
| 链路状态数据库 + LSA 泛洪 | 区域划分(Area),全网单一区域 |
| 序号与 Age 判定新旧 | DR/BDR 选举 |
| Dijkstra 最短路径计算 | 协议报文的完整 TLV 编码 |
| 链路开销度量 | 认证字段(由 Noise 层承担) |

#### 8.8.1 度量与平滑

链路开销来自延迟采样,经过 **EWMA 平滑**:

```
ewma_new = α × sample + (1 - α) × ewma_old
```

`α` 取较小值(约 0.2),使单次抖动不会立刻改变路由。路径抖动会导致 TCP 连接在两条路径间反复迁移,进而乱序,因此平滑是**必需**而非优化。

#### 8.8.2 路由策略

| 策略 | 选路依据 | 适用 |
|---|---|---|
| `shortest` | 跳数最少 | 结构稳定的固定拓扑 |
| `latency` | 平滑后延迟最低 | 跨地域、链路质量差异大 |
| `weighted` | 管理员指定权重/成本 | 需要让流量走特定链路(如计费链路) |

#### 8.8.3 多路径

同一目的节点存在多条等价路径时,按流(五元组哈希)分流,而非按包分流。按包分流会造成同一 TCP 流的乱序,按流分流可保留单流顺序性同时利用多路径带宽。

### 8.9 状态机

#### 8.9.1 Peer 状态机

```
        ┌──────────┐
        │  Idle    │  发现对端(来自 LSA / 配置 / 控制面)
        └────┬─────┘
             │ 发起连接
             ▼
        ┌──────────┐
        │Handshake │  Noise IK 1-RTT
        └────┬─────┘
       成功  │  失败(重试耗尽)
             ▼         └──────────▶ ┌──────────┐
        ┌──────────┐               │  Failed  │
        │ Probing  │  打洞/探测直连 └──────────┘
        └────┬─────┘
     打洞成功 │  打洞失败/超时
             ▼        └───────────┐
        ┌──────────┐             ▼
        │ Direct   │◀──打洞成功──┐┌──────────┐
        └────┬─────┘            ││ Relayed  │
             │ 打洞失败/超时      │└────┬─────┘
             ▼                  │     │ 定期重试打洞
        ┌──────────┐            │◀────┘
        │ Relayed  │────────────┘
        └────┬─────┘
             │ 3 个 keepalive 周期无响应
             ▼
        ┌──────────┐
        │ Degraded │  仍可用,但不再作为首选路径
        └────┬─────┘
             │ 6 个周期无响应
             ▼
        ┌──────────┐
        │   Dead   │  从路由表移除,触发重新收敛
        └──────────┘
```

##### 8.9.1.1 状态枚举(实现契约)

代码中的 `State` 取值与上图严格对应,状态名是**项目内契约**(§2),
不允许各模块自行发明别名:

| 状态 | 含义 |
|---|---|
| `Idle` | 已发现对端但尚未发起连接 |
| `Handshake` | 正在执行 Noise IK 握手 |
| `Probing` | 握手完成,正在打洞/探测直连路径 |
| `Direct` | 已建立直连路径 |
| `Relayed` | 经中继转发,后台周期性尝试重新打洞 |
| `Degraded` | 连续一个降级周期无响应,仍可用但不再作为首选路径 |
| `Dead` | 连续多个周期无响应,应从路由表移除 |
| `Failed` | 握手重试耗尽,无法建立会话 |

> 命名风格说明:`Handshake`(原形)、`Probing`(进行式)、`Relayed`(过去式)
> 来自本文档的原始状态图,不完全统一。为保持与契约一致,**现阶段保持这些名字**,
> 详见 `docs/TECHDEBT.md`。

#### 8.9.2 节点生命周期

`Init → ConfigLoaded → IdentityReady → IngressUp → TransportUp → MeshJoined → Running → Shutdown`

任一阶段失败都输出明确的阶段名与原因,便于排错时定位(例如 `IngressUp` 失败通常是权限不足或 `/dev/net/tun` 缺失)。

#### 8.9.3 Phase 1a 实现现状与临时限制

截至 Phase 1a,`core/peer` 已实现 §8.9.1 的状态机与常量,但**部分状态是占位**,
真实网络能力尚待 Phase 2 补齐:

| 能力 | Phase 1a 状态 | 说明 |
|---|---|---|
| `Idle → Handshake → Direct` | ✅ 真实可用 | 两节点完成 Noise IK 握手并直接互发加密数据 |
| `Probing` / `Relayed` | ⚠️ 状态占位 | `Driver.StartPunch` 无真实打洞逻辑;`Probing → Relayed` 仅在打洞超时被触发,不产生真实中继流量 |
| 对端公钥 | ⚠️ 手工注入 | Noise IK 要求发起方预知响应方公钥。Phase 1a 无控制面/发现通道,由调用方通过 `node.PeerSpec{Addr, PublicKey}` 显式注入。Phase 2 控制面完成后自动下发 |
| 重新打洞 | ⚠️ 状态流转可用 | `RepunchInterval` 节流与状态转换已实现,但底层无真实打洞可重试 |
| 路由 / LSA / 中继 / ACL | ⚠️ 部分 | 路由与 LSA 属 Phase 1c(见 §8.9.4);中继与 ACL 属 Phase 2 及以后 |

**结论**:Phase 1a 的边界是"两个节点能完成 Noise 握手、建立加密会话、互发加密
数据包"。打洞、中继、控制面下发公钥均在 Phase 2 落地;在那之前,`Probing` 与
`Relayed` 只承载状态机语义,不代表真实可达的传输路径。

#### 8.9.4 Phase 1c 实现现状(OSPF 子集)

截至 Phase 1c,`core/route` 已实现 §8.6.2 / §8.8 的链路状态路由子集:

| 能力 | Phase 1c 状态 | 说明 |
|---|---|---|
| LSA 编解码 | ✅ 已实现 | 线格式见 `core/route/lsa.go`,序号 + Age 判定新旧 |
| 拓扑库 | ✅ 已实现 | `TopologyDB`,版本号驱动重算,超龄 LSA 自动清除 |
| LSA 泛洪 | ✅ 已实现 | 收到更新的 LSA 转发给除来源外的邻居,序号去重防环路 |
| Dijkstra | ✅ 已实现 | `Compute` 生成转发表;单向声明的链路不构成邻接 |
| 策略 | ⚠️ 部分 | `shortest` / `latency` 可用;`weighted` 仅保留接口(Phase 5) |
| 链路探测 | ✅ 已实现 | Probe/ProbeAck + EWMA 平滑 RTT |
| 透明多跳 | ✅ 已实现 | A-B-C 直线拓扑下 A 可经 B 中转至 C |
| 故障收敛 | ✅ 已实现 | 中间节点掉线后其 LSA 超龄被清除,依赖它的路由消失 |
| E2EE 中继 | ⚠️ 逐跳解密 | Phase 1c 采用逐跳转发,中间节点可见明文(见 TECHDEBT #7) |

**验收边界**:三个节点直线拓扑(A—B—C,A 与 C 不直连)能通过 LSA 收敛,
A 经 B 中转到 C;B 掉线后 A、C 重新收敛为不可达。打洞、真实中继、多路径
属 Phase 2/5。

### 8.10 安全考虑

| 威胁 | 对策 |
|---|---|
| 中间人窃听 | Noise IK + ChaCha20-Poly1305 E2EE,中继不可解密 |
| 重放攻击 | Sequence 滑动窗口,窗口外丢弃 |
| 身份伪造 | NodeID 由公钥哈希派生,无法在不持有私钥的情况下伪造 |
| DoS 放大 | 握手限速 + token 校验前不分配状态;打洞包随机载荷抗指纹 |
| 中继滥用 | relay 侧 ACL 与流量配额 |
| 配置泄漏 | `secrets/*.toml` 强制 0600,启动时校验权限 |
| 路由劫持 | LSA 携带 Originator 签名校验,非法 LSA 丢弃 |
| 降级攻击 | 协议 `Ver` 固定参与 AAD 校验,防止版本被改写 |

### 8.11 协议常量

| 常量 | 值 | 说明 |
|---|---|---|
| `Magic` | `0x4C`(`L`) | 包头魔数 |
| `ProtoVersion` | `1` | 当前协议版本 |
| `HeaderSize` | `48` | 固定头长度(字节) |
| `DefaultPort` | `11010` | 默认监听端口(UDP/TCP) |
| `DefaultSubnet` | `10.126.126.0/24` | 默认虚拟网段 |
| `KeepaliveInterval` | `20s` | 保活周期 |
| `PeerDegradedAfter` | `60s` | 降级判定 |
| `PeerDeadAfter` | `120s` | 死亡判定 |
| `DefaultMTU` | `1380` | 虚拟 MTU |
| `PunchTimeout` | `5s` | 打洞总超时 |
| `RepunchInterval` | `60s` | 中继状态下重打洞周期 |
| `CommitTimeout` | 未定义 | — |
| `LSAFloodInterval` | `10s` | LSA 周期泛洪 |
| `LSAMaxAge` | `300s` | LSA 失效时间 |
| `HandshakeTimeout` | `5s` | 单次握手超时 |
| `HandshakeMaxRetry` | `5` | 握手最大重试次数 |
| `EWMAAlpha` | `0.2` | 延迟平滑系数 |

---

## 9. 传输协议(18 种)

所有传输实现统一接口,由注册表按名称或自动评分选取。

### 9.1 统一接口

```go
// Transport 是所有传输协议必须实现的接口。
type Transport interface {
    // Name 返回注册名,与配置中的 [transport].protocols 项一一对应。
    Name() string
    // Dial 主动连接到指定对端。
    Dial(ctx context.Context, peer PeerAddr) (Conn, error)
    // Listen 监听入站连接。
    Listen(ctx context.Context, addr string) (Listener, error)
    // Capabilities 声明该传输的能力(可靠性、是否支持多路复用等)。
    Capabilities() Caps
}
```

`Caps` 至少包含:`Reliable bool`(是否自带重传)、`Ordered bool`(是否保序)、`Multiplexed bool`(是否原生多路复用)、`MTU int`(建议 MTU)。

数据面依据 `Caps` 决定是否启用分片、是否需要序号重排、是否复用一个底层连接。

### 9.2 协议清单

| # | 协议 | 承载 | 特点 | 主要场景 |
|---|---|---|---|---|
| 1 | UDP | UDP | 延迟最低,无重传 | 默认首选,公网直连 |
| 2 | TCP | TCP | 穿透友好,可靠 | UDP 被封锁时的兜底 |
| 3 | QUIC | UDP | 加密 + 多路复用 + 0-RTT | 高丢包广域网 |
| 4 | TLS | TCP | 伪装成 HTTPS 流量 | 严格防火墙环境 |
| 5 | WS | TCP | WebSocket,握手像 HTTP | 仅放行 HTTP 的代理环境 |
| 6 | WSS | TLS | WebSocket over TLS | 企业代理 + 加密 |
| 7 | HTTP/2 | TCP/TLS | 多路复用 + 头部压缩 | 与现有 HTTP/2 基建共存 |
| 8 | HTTP/3 | QUIC | 基于 QUIC 的 HTTP | 弱网 + HTTP 基建 |
| 9 | gRPC | HTTP/2 | 双向流 + 强类型 | 与云原生控制面集成 |
| 10 | KCP | UDP | 激进重传,弱网吞吐好 | 高丢包跨境链路 |
| 11 | DTLS | UDP | 数据报 TLS | 需要 TLS 语义但不想用 TCP |
| 12 | WireGuard | UDP | 与 WG 生态互操作 | 与现有 WG 网络对接 |
| 13 | SSH | TCP | 复用 22 端口 | 只开放 SSH 的受限环境 |
| 14 | ICMP | ICMP | 伪装成 ping | 只放行 ICMP 的网络 |
| 15 | DNS | UDP/TCP | 伪装成 DNS 查询 | 只放行 53 端口的网络 |
| 16 | Unix Socket | 本地 | 零网络栈开销 | 同机多进程互联 |
| 17 | VSOCK | 本地 | 虚拟机 ↔ 宿主 | VM/容器沙箱场景 |
| 18 | 共享内存 | 本地 | 延迟最低 | 同机高频通信 |

### 9.3 选择策略

```
配置显式指定 → 使用指定协议(可给多个,按序尝试)
未指定      → 自动评分:
              公网可达性(UDP 通?) > 延迟采样 > 丢包率 > 历史成功率
              同机对端优先本地传输(共享内存 > Unix Socket > 回环 UDP)
```

**隧道类传输的取舍**:ICMP / DNS / SSH 隧道属于"最后手段",其吞吐与延迟明显劣于原生 UDP。自动评分会给它们较低的优先级,只有在探测到其他协议全部不可用时才启用。

---

## 10. 配置系统

配置系统是 Limesh 工程复杂度最高的模块之一,因为它必须同时满足:**人类可读、机器可覆盖、集群可组合、运行时可热更**。

### 10.1 四层优先级

合并顺序(后者覆盖前者):

```
默认值(Default) < 文件字段(TOML) < 内嵌 [cli] 块 < 环境变量(ENV) < 命令行(CLI)
```

注意 `[cli]` 块的位置:**它位于默认值与环境变量之间**,即写进配置文件的 CLI 语法会被环境变量和命令行覆盖。这样设计使"配置文件的意图"有一个明确的、低于运行时覆盖的层级。

优先级示例:

| 来源 | `log` 值 |
|---|---|
| 默认值 | `info` |
| 文件字段 `[log].level = "warn"` | `warn` |
| 内嵌 `[cli] log = "debug"` | `debug` |
| `MESH_LOG=error` | `error` |
| `--log trace` | `trace` |

### 10.2 多文件 + 通配符 + include

```bash
# 单文件
mesh node -c /etc/mesh/config.toml

# 多文件,按给出顺序合并
mesh node -c base.toml -c override.toml

# 通配符:匹配单层
mesh node -c 'conf.d/*.toml'

# 递归通配符:匹配多层
mesh node -c 'conf.d/**/*.toml'
```

配置文件内可再引用其它文件:

```toml
include = ["conf.d/*.toml", "secrets/*.toml"]
```

规则:

- `include` 中的相对路径**相对于当前文件所在目录**解析,而非进程工作目录。
- 通配符展开结果按字典序排序后合并,保证不同机器上顺序一致。
- 检测到循环 include 立即报错并退出,不静默忽略。
- 重复 include 同一文件只生效一次(按解析后的绝对路径去重)。

### 10.3 内嵌 [cli] 块

`[cli]` 块允许在配置文件里写 flag 语法,使"打算怎么跑"与"配置文件"共处一地:

```toml
[cli]
log   = "info"
topo  = true
```

语义:

- 键名与 flag 名**完全一致**(去掉 `--` 前缀)。
- 布尔 flag 写 `true` 表示启用。
- 数组用 TOML 数组表示。

这样做的收益:一份"生产环境该怎么跑"的知识可以随配置一起版本化,而不是散落在 systemd unit 或运维手册里。

### 10.4 合并规则

| 类型 | 规则 |
|---|---|
| map(表) | **递归合并**,同键继续下钻 |
| 数组 | **追加**(前者在前,后者在后),不去重 |
| 标量 | **覆盖**(高优先级胜出) |

数组追加是有意设计:ACL 规则、对端列表、传输协议列表这类配置,期望"基础配置 + 增量覆盖"而非"整体替换"。

**清空继承值必须用 `reset`,裸写 `[]` 不清空。** 这一点容易误判,单独说明如下。

### 10.4.1 reset:显式清空继承来的值

`reset` 是一个**顶层字段**,与 `include` 并列,取值为需要清空的字段路径列表:

```toml
# 清空 listen 和 peers,然后重新定义
reset = ["listen", "peers"]

[[listen]]
addr = "udp://0.0.0.0:11010"
```

语义:

| 规则 | 说明 |
|---|---|
| 生效时机 | 在**本文档参与的合并**开始前,先丢弃这些路径从低优先级来源继承的值 |
| 作用范围 | 只清空**低优先级来源**的值;更高优先级来源(ENV / CLI)不受影响 |
| 字段路径 | 用点号分隔,如 `reset = ["routing.weight", "security.acl.rules"]` |
| 表与数组 | 对 map 清空整表,对数组清空整列表 |
| 不存在的路径 | 静默忽略,不报错 |
| 重复声明 | 多个文件中都写 `reset` 时,结果取**并集** |
| 多文件场景 | 每个文件的 `reset` 只作用于该文件**之前**已合并的结果 |
| **响应级别** | 不适用。`reset` 是合并期元字段,热重载时随加载流程重新求值;被它清空后重新定义的字段,响应级别按字段自身的定义处理 |

`reset` 与 `include` 同属**合并期元字段**:它们在配置加载阶段求值,不参与运行时的 `hot` / `reconnect` / `restart` 分级,也不会出现在 `mesh config diff` 的变更清单中。热重载会完整重跑加载流程(读取文件 → 展开 include → 执行 reset → 合并 → 生成新快照),因此 `reset` 的改动能够生效,但**触发 Apply 的始终是被清空后重新定义的实际字段**。

需要注意的是,`reset` 可以**间接**导致 `restart` 级行为:若它清空并重新定义的是 `network.name`、`network.secret` 这类 `restart` 级字段,结果仍是拒绝热重载并标记 `PendingRestart`。级别始终由字段自身决定,不由 `reset` 决定。

**为什么不是裸写 `[]`**

`[]` 参与合并时是"追加零个元素",语义上等价于"什么都不做",因此继承值原样保留:

```toml
# base.toml
[peers]
relay = ["1.2.3.4:11010"]
```

```toml
# override.toml
[peers]
relay = []          # ❌ 继承值仍在:["1.2.3.4:11010"]
```

```toml
# override.toml
reset = ["peers"]   # ✅ 先清空 peers,后面的定义才是最终值

[peers]
relay = ["5.6.7.8:11010"]
```

这样设计的原因:数组语义统一为"追加"后,`[]` 就没有表达"清空"的能力了。与其给 `[]` 附加一个与加法语义相悖的特殊含义,不如引入一个显式的、可读的 `reset` 字段——**"清空"是一个需要被看见的意图**,不应该藏在空数组里。

**与优先级的交互**

`reset` 只清空**低优先级来源**,不能越过 ENV 与 CLI:

```toml
# /etc/mesh/config.toml
reset = ["log"]
```

```bash
MESH_LOG=debug mesh node -c /etc/mesh/config.toml
```

结果:`log.level = "debug"`。`reset` 清掉了文件与 `[cli]` 块里的 `[log]`,但环境变量仍然生效——因为 ENV 优先级高于文件。

### 10.5 热重载

三个触发入口:

1. **文件监听**:fsnotify 监听所有被合并的配置文件(含 include 展开结果)。
2. **信号**:`SIGHUP` 强制重新加载。
3. **Admin API**:`POST /api/v1/config/reload` 远程触发。

流程:

```
触发 → 重新解析全部来源 → 生成新 Config 快照
     → 与旧快照逐模块 diff → 按 hot / reconnect / restart 分级
     → 逐模块 Apply
         ├─ 全部成功 → 原子替换快照,写审计日志
         └─ 任一失败 → 回滚已 Apply 的模块 → 保留旧快照 → 报错
```

关键约束:

- **原子替换**:运行期读到的永远是一份完整快照,不存在"改了一半"的中间态。
- **失败回滚**:Apply 失败的模块必须能恢复到旧值;因此每个模块的 `Apply` 都要实现 `Revert`。
- **restart 级变更**不自动重启进程,而是标记为 `PendingRestart`,由 `mesh config diff` / `mesh status` 提示需要重启。
- 热重载失败**不得**导致进程退出,只能记录错误并保持旧配置继续服务。

### 10.6 权限检查

启动时与热重载时都进行检查:

| 检查项 | 要求 | 违规处理 |
|---|---|---|
| `secrets/*.toml` 文件权限 | 必须为 `0600` | 拒绝启动 / 拒绝重载,输出明确错误 |
| 密钥文件所在目录 | 建议 `0700` | 警告 |
| 配置文件是否为符号链接 | 允许,但记录真实路径 | 仅记录 |
| 配置文件属主 | 应为运行用户 | 警告 |

设计理由:配置文件里会出现网络密钥,而密钥一旦被同机其他用户读取,整个虚拟网络的身份隔离就失效了。因此 `secrets/` 目录下的文件做**强制**校验而非警告。

### 10.7 来源追踪

每个生效的配置项都记录**最终值的来源**:

```
network.secret   ← /etc/mesh/secrets/prod.toml:3
network.mtu      ← MESH_NETWORK_MTU
log.level        ← --log(命令行)
route.strategy   ← 默认值
```

暴露方式:

```bash
mesh config show --sources
mesh config show --sources --format json
```

这是排错的关键工具:绝大多数"配置没生效"的问题,答案都是"它被更高优先级的来源覆盖了"。

---

## 11. 单二进制角色

同一个 `mesh` 二进制通过子命令切换角色。角色是**运行时构造的**,不是编译期裁剪的。

```bash
mesh node          # 节点(默认角色)
mesh relay         # 中继(零权限)
mesh controller    # 控制面
mesh gateway       # 网关
mesh exit          # 出口节点
mesh config        # 配置管理
mesh peer          # 对端管理
mesh route         # 路由管理
mesh topo          # 拓扑
mesh status        # 状态
mesh version       # 版本
```

| 角色 | 职责 | 权限需求 | 典型部署 |
|---|---|---|---|
| `node` | 加入虚拟网络,收发 IP 包 | 按 Ingress 模式 | 每台要组网的机器 |
| `relay` | 为无法直连的节点转发流量 | **零权限** | 公网 VPS |
| `controller` | 分发网络配置、同步拓扑、下发 ACL | 零权限 | 中心节点或自托管 |
| `gateway` | 虚拟网络与物理网络互通的网关/入口 | 需要转发能力 | 机房边界 |
| `exit` | 作为默认出口访问公网 | 需要 NAT/转发 | 有公网出口的节点 |

设计要点:

- **`relay` 零权限**是刻意的。中继常部署在共享环境或最小化 VPS 上,要求 root 会显著抬高部署门槛。中继不创建 TUN,只做 `DstNodeID` 查表转发。
- **`controller` 不参与数据面**。控制面挂了只影响新节点加入与配置变更,已建立的 P2P 连接继续工作。这是去中心化设计的重要性质:控制面不是单点故障。
- **`gateway` 与 `exit` 的区别**:`gateway` 是把虚拟网络暴露给物理网络(入方向),`exit` 是把物理网络暴露给虚拟网络(出方向)。

---

## 12. 三种 Ingress 模式

Ingress 是"虚拟网络与本地网络栈之间的接口"。三种模式对应三种权限与能力组合。

| 模式 | 实现 | 需要 root | 能力 | 适用 |
|---|---|---|---|---|
| `tun` | wireguard-go/tun 创建 TUN 设备 | 是 | 完整三层,内核原生路由 | 服务器、路由器、有 root 的机器 |
| `no-tun` | gVisor netstack 用户态协议栈 | 否 | 用户态栈 + SOCKS5 + 端口转发 | 共享服务器、CI、受限容器 |
| `auto` | 优先 `tun`,失败自动降级到 `no-tun` | 视情况 | 两者取其一 | 默认推荐 |

### 12.1 模式差异

```
tun 模式:
  应用 ── 内核路由表 ── TUN 设备 ── Ingress ── 数据面 ── 对端
  ✓ 任意应用透明使用,无需改配置
  ✓ 支持 ping、端口转发、任意协议
  ✗ 需要 root / CAP_NET_ADMIN

no-tun 模式:
  应用 ── SOCKS5 (127.0.0.1:1080) ── gVisor netstack ── 数据面 ── 对端
       └─ 端口转发 (本地端口 → 虚拟 IP:端口)
  ✓ 零权限
  ✓ 单进程/单工具可精确控制走不走虚拟网络
  ✗ 不透明,需要应用支持 SOCKS5 或使用端口转发
  ✗ 无法直接 ping(除非用 TCP 探测)
```

### 12.2 自动降级

`auto` 模式的判定顺序:

1. 尝试打开 `/dev/net/tun` 并创建接口。
2. 失败(权限不足 / 设备不存在 / 已被占用)→ 打印一条**明确的**降级提示,转入 `no-tun`。
3. 降级后仍启动成功即视为正常启动,退出码为 0。

降级提示必须包含:失败原因、降级后的能力边界、如何启用完整模式。静默降级是禁止的——用户必须知道自己处在哪个模式。

### 12.3 Phase 1b 实现现状与临时限制

截至 Phase 1b,`core/device` 与 `core/ingress` 已实现 TUN 模式:

| 能力 | 状态 | 说明 |
|---|---|---|
| `tun` 模式 | ✅ 已实现 | `core/device` 跨平台打开 TUN;`core/ingress` 做 IP 包搬运 |
| TUN → 数据面 | ✅ 已实现 | TUN 读到的 IP 包经 `Outbound()` 交给 node 数据面加密发出 |
| 数据面 → TUN | ✅ 已实现 | 解密后的 IP 包经 `Ingress.Deliver()` 写回 TUN |
| 权限错误提示 | ✅ 已实现 | 权限不足时给出按平台(含 setcap/Docker/K8s)的修复指引 |
| `no-tun` 模式 | ❌ 未实现 | 返回 `ErrNotImplemented`,Phase 3 落地 |
| `auto` 降级 | ⚠️ 部分 | 权限不足时打印明确的降级提示,但 no-tun 未实现,故仍返回错误而非真正降级 |
| 路由 | ✅ OSPF 子集 | Phase 1c 已实现链路状态泛洪 + Dijkstra + 转发表,支持 A-B-C 透明多跳。详见 §8.9.4 |

**平台差异**:Linux 用 `mesh0`、macOS 用系统分配的 `utun*`、Windows 用 Wintun 的
`mesh`、FreeBSD/OpenBSD 用 `tun*`。设备名解析优先级:`tun.name` > `ingress.interface`
> 默认 `mesh0`(见 `Config.EffectiveTUNName`)。

**验收边界**:两个(且仅两个)节点各自创建 TUN、完成 Noise 握手后,能通过虚拟
IP 互 ping。此边界之外(多节点、打洞、中继)均属后续阶段。

---

## 13. 权限模型

| 平台 | `node` | `relay` |
|---|---|---|
| Linux | `setcap cap_net_admin,cap_net_raw+ep /usr/local/bin/mesh` | 无需 |
| macOS | `sudo` | 无需 |
| Windows | 管理员 | 无需 |
| Docker | `NET_ADMIN` + `/dev/net/tun` | 无需 |
| K8s | `securityContext`(capabilities + privileged 视 CNI 而定) | 无需 |
| Android | 无需 root(系统 VPNService) | — |
| iOS | 无需 root(NetworkExtension) | — |

### 13.1 Linux capabilities

推荐用最小 capability 而非 root:

```bash
sudo setcap cap_net_admin,cap_net_raw+ep /usr/local/bin/mesh
```

- `cap_net_admin`:创建/配置 TUN 设备、操作路由表。
- `cap_net_raw`:原始套接字,ICMP 隧道与部分 NAT 探测需要。

注意:二进制升级后 capability 会丢失(setcap 是打在文件上的),因此部署脚本应在每次安装后重新执行 setcap。

### 13.2 无 root 时自动降级

无 root、无 capability、无 `/dev/net/tun` 时,节点**不退出**,自动切换到 no-tun 模式:

```
gVisor 用户态栈 + SOCKS5(默认 127.0.0.1:1080)+ 端口转发
```

这条路径让 Limesh 能在:共享主机、CI runner、受限容器、公司笔记本上直接运行,是相对同类工具的重要差异点。

---

## 14. 平台支持

目标:**30+ 平台组合**,全部由 `CGO_ENABLED=0` 交叉编译产出。

| 系统 | 架构 |
|---|---|
| Linux | amd64, arm64, armv5, armv6, armv7, 386, mips, mipsle, mips64le, riscv64, loong64, ppc64le, s390x |
| macOS | amd64, arm64 |
| Windows | amd64, arm64, 386 |
| FreeBSD | amd64, arm64 |
| OpenBSD | amd64, arm64 |
| NetBSD | amd64, arm64 |
| DragonFly | amd64, arm64 |
| Solaris | amd64 |
| Illumos | amd64 |
| AIX | ppc64 |
| Android | arm64(后期,gomobile) |
| iOS | arm64(后期,gomobile) |

### 14.1 构建矩阵

```
GOOS × GOARCH 组合 → 逐个 go build → 打包 tar.gz / zip(Windows)
```

注意点:

- `armv5/6/7` 需要设置 `GOARM`,CI 中显式声明为三个独立目标。
- `mips` 系需要 `GOMIPS=softfloat`(路由器多数为软浮点)。
- `Android/iOS` 不走标准 `go build`,使用 `gomobile bind` 产出 AAR / XCFramework。

---

## 15. Docker 部署

镜像 `limesh/mesh` 采用多架构构建(`linux/amd64`、`linux/arm64`、`linux/arm/v7`)。

### 15.1 镜像设计

```dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/mesh ./cmd/mesh

FROM scratch
COPY --from=build /out/mesh /mesh
ENTRYPOINT ["/mesh"]
CMD ["node"]
```

要点:

- `scratch` 基础镜像 → 镜像体积约 20 MB 量级,无 shell、无 libc,**攻击面最小**。
- 无 shell 意味着不能用 `docker exec sh` 进容器调试;排错依赖 `mesh status` / `mesh topo` 命令与容器日志。
- 健康检查使用 `mesh status`(进程内子命令,不需要额外二进制)。

### 15.2 运行要求

| 角色 | 参数 |
|---|---|
| `node`(TUN) | `--cap-add=NET_ADMIN --device=/dev/net/tun` |
| `node`(no-tun) | 无特殊要求 |
| `relay` | 无特殊要求,只需暴露 `11010/udp` 与 `11010/tcp` |

### 15.3 Compose

典型三段式:relay + 两个 node。relay 使用 `network_mode: host` 以获得真实公网候选地址(容器 NAT 会干扰打洞)。

---

## 16. K8s 部署

### 16.1 DaemonSet

节点以 DaemonSet 形式部署到每个需要组网的 Node 上,共享宿主机网络命名空间(`hostNetwork: true`)以保证打洞可用。

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: mesh-node
  namespace: kube-system
spec:
  selector:
    matchLabels: { app: mesh-node }
  template:
    metadata:
      labels: { app: mesh-node }
    spec:
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      containers:
        - name: mesh
          image: limesh/mesh:latest
          args: ["node", "-c", "/etc/mesh/config.toml"]
          securityContext:
            capabilities:
              add: ["NET_ADMIN", "NET_RAW"]
          volumeMounts:
            - { name: config, mountPath: /etc/mesh }
            - { name: devnet, mountPath: /dev/net }
            - { name: state,  mountPath: /var/lib/mesh }
      volumes:
        - name: config
          configMap: { name: mesh-config }
        - name: devnet
          hostPath: { path: /dev/net }
        - name: state
          hostPath: { path: /var/lib/mesh }
```

### 16.2 设计取舍

| 选择 | 理由 |
|---|---|
| `hostNetwork: true` | 容器网络命名空间会引入额外 NAT,显著降低打洞成功率 |
| ConfigMap 提供配置 | 配置随集群版本化,避免手工改机器 |
| 状态目录用 hostPath | 保留 NodeID 与密钥,Pod 重建后身份不变(否则每次重建都在网络里留下僵尸节点) |
| 不使用 privileged | 能用 capabilities 就不用 privileged,减少爆炸半径 |

### 16.3 多集群互联

跨 K8s 集群互联时,每个集群部署一个 relay(或复用出口节点),其余节点通过该 relay 收敛。这样跨集群流量有明确的汇聚点,便于审计与限流。

---

## 17. 许可证与合规

- **本项目许可证**:Apache License 2.0。
- **许可证兼容性铁律**:所有依赖必须与 Apache 2.0 兼容。禁止引入 GPL / AGPL / SSPL 系依赖。
- **CI 检查**:自动扫描依赖许可证,发现不兼容项即阻断合并。
- **贡献者**:提交 PR 即视为同意以 Apache 2.0 授权其贡献(采用 DCO 签名,`Signed-off-by`)。

依赖许可证速查(当前选型):

| 依赖 | 许可证 | 兼容 |
|---|---|---|
| koanf | MIT | 是 |
| cobra / pflag | Apache-2.0 | 是 |
| wireguard-go/tun | MIT | 是 |
| gvisor | Apache-2.0 | 是 |
| quic-go | MIT | 是 |
| xtaci/kcp-go | MIT | 是 |
| pion/stun, pion/ice | MIT | 是 |
| zap | MIT | 是 |
| prometheus/client_golang | Apache-2.0 | 是 |

---

## 18. 路线图

| 阶段 | 周期 | 交付内容 |
|---|---|---|
| **Phase 0** | 2 周 | 骨架、ZCPacket、UDP 传输、Config、单二进制 main、两节点互通 |
| **Phase 1** | 6 周 | Noise 握手、OSPF 路由、Peer 状态机、TUN、CLI 完整命令树 |
| **Phase 2** | 6 周 | STUN/ICE、中继、UPnP、端口预测、QUIC/TCP/TLS/WS/WSS/KCP |
| **Phase 3** | 4 周 | gVisor 用户态、SOCKS5、Prometheus、拓扑 API |
| **Phase 4** | 4 周 | E2EE、ACL、密钥轮换、Docker、K8s、WireGuard 互操作 |
| **Phase 5** | 4 周 | 延迟优先路由、多路径、出口节点、MagicDNS、Web 面板 |
| **Phase 6** | 后期 | SSH/ICMP/DNS 隧道、Unix Socket/VSOCK、gomobile、GUI |

### 18.1 阶段出口标准

| 阶段 | 出口条件(必须全部满足) |
|---|---|
| Phase 0 | 两台机器通过 UDP 传输互换 IP 包并通过 `ping` 验证;`mesh version` / `mesh status` 可用 |
| Phase 1 | 三节点网络自动收敛路由;任一节点故障后 10 秒内重新收敛;TUN 模式在 Linux 可用 |
| Phase 2 | 对称 NAT 下打洞成功率有实测数据;打洞失败自动中继且不掉线 |
| Phase 3 | 无 root 环境下完成端到端连通;`/metrics` 可被 Prometheus 抓取 |
| Phase 4 | 中继抓包无法还原明文;ACL 拒绝事件写入审计日志 |
| Phase 5 | 延迟策略相对最短路径策略有可测量的端到端延迟改善 |
| Phase 6 | 移动端可加入网络并保持后台存活 |

---

## 19. 构建与发布

### 19.1 本地构建

```bash
# 开发构建
make build            # → ./bin/mesh

# 交叉编译单平台
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 make build

# 全平台
make build-all        # → ./dist/ 下按 OS-ARCH 命名
```

### 19.2 构建约束

| 约束 | 值 |
|---|---|
| `CGO_ENABLED` | `0`(强制) |
| Go 版本 | 1.22+ |
| 编译标志 | `-trimpath -ldflags "-s -w -X main.version=..."` |
| 产物 | `mesh`(Linux/macOS)、`mesh.exe`(Windows) |
| 打包 | `tar.gz`(类 Unix)、`zip`(Windows) |
| 校验 | 每个产物附带 `sha256sum` |

### 19.3 发布流程

```
tag vX.Y.Z
  → CI 触发全平台矩阵构建
  → 单元测试 + 竞态检测(go test -race)
  → 依赖许可证扫描
  → 依赖方向断言(分层规则)
  → 生成产物 + 校验和
  → 构建多架构 Docker 镜像并推送
  → 创建 GitHub Release,附变更日志
```

### 19.4 版本语义

- `vMAJOR`:协议不兼容变更(ZCPacket 格式或握手流程变更)。
- `vMINOR`:新增传输协议、新角色、新配置项(向后兼容)。
- `vPATCH`:修复、性能、文档。

协议版本(`ProtoVersion`)与发布版本解耦:同一发布版本可支持多个协议版本以支持灰度升级。

---

## 20. 风险与对策

| # | 风险 | 影响 | 对策 |
|---|---|---|---|
| 1 | 对称 NAT 下打洞失败率高 | 用户感知为"慢"或"连不上" | 并发竞赛策略 + 端口预测 + 生日悖论;失败快速降级中继;上线前统计各类 NAT 类型成功率 |
| 2 | 18 种传输导致维护成本失控 | 代码腐化、缺陷密度上升 | 统一接口 + `Caps` 声明;每种传输有独立测试夹具;隧道类传输明确标记为"最后手段" |
| 3 | 热重载部分生效导致状态不一致 | 难以复现的线上故障 | 快照原子替换 + 逐模块 Revert + 全成功才提交;restart 级变更不自动应用 |
| 4 | 中继带宽成为瓶颈与成本中心 | 用户体验下降、运营成本高 | 中继只作为兜底;直连成功后自动回切;中继侧配额与 ACL;地理就近选择 |
| 5 | 配置合并语义复杂,用户误判 | "配置没生效"类工单 | 来源追踪(`--sources`)+ 明确日志打印每个覆盖点 |
| 6 | 无 root 模式能力受限,用户预期错位 | 认为工具"不工作" | 降级时显式打印能力边界;文档明确列出 no-tun 的能做与不能做 |
| 7 | 30+ 平台中冷门架构无法真机测试 | 特定平台二进制不可用 | 至少保证可编译 + 启动冒烟;冷门架构标注为"社区验证" |
| 8 | 分层规则被新贡献者无意打破 | 架构腐化,移动端预留失效 | CI 强制依赖方向断言;PR 模板包含设计原则自检清单 |
| 9 | 密钥泄漏 | 整个虚拟网络身份被冒充 | `secrets/*.toml` 强制 0600;支持在线轮换;审计日志记录密钥使用 |
| 10 | 过度设计(18 种传输一次全做) | 交付延期 | 严格按路线图分期;Phase 2 只交付 6 种高频传输 |

---

## 附录 A:关键设计决策速查(ADR 摘要)

| 决策 | 选择 | 主要理由 | 撤销条件 |
|---|---|---|---|
| 包头格式 | 固定 48 字节 | 零拷贝解析、缓冲复用 | 需要 >16 字节寻址空间时 |
| 数据面可靠性 | 不做重传 | 避免双重可靠性,由内层协议负责 | 出现必须可靠的非 IP 载荷 |
| 路由度量 | EWMA 平滑延迟 | 抑制路径抖动导致的乱序 | 抖动量级超过平滑代价 |
| 配置格式 | 仅 TOML | 表达力足够、解析库成熟、避免多格式歧义 | 出现无法用 TOML 表达的配置需求 |
| include 路径基准 | 相对当前文件 | 可重定位、可版本化 | — |
| 中继权限 | 零权限 | 降低部署门槛 | 出现必须提权的转发需求 |
| Ingress 默认 | `auto` | 兼顾完整能力与可用性 | — |
| 单二进制 | 全部角色 | 分发与版本一致性 | 移动端体积不可接受时(部分角色可裁剪) |

## 附录 B:术语表

| 术语 | 含义 |
|---|---|
| 节点(Node) | 运行 `mesh node` 的网络成员 |
| 中继(Relay) | 为无法直连的节点转发流量的角色,零权限 |
| 控制面(Controller) | 分发配置、同步拓扑的角色,不参与数据转发 |
| 对端(Peer) | 从当前节点视角看到的另一个节点 |
| 直连(Direct) | 双方直接收发,无第三方转发 |
| 中继路径(Relayed) | 经 relay 转发的路径 |
| 打洞(Punch) | 通过并发探测在同一 NAT 映射上建立直连 |
| 接入层(Ingress) | TUN / netstack / SOCKS5 / 端口转发 |
| LSA | 链路状态通告,路由收敛的基本单位 |
| 虚拟 IP | 从 NodeID 派生的网络内地址 |
| NodeID | `SHA256(pubkey)[:16]`,节点身份 |
| 热重载 | 不重启进程应用配置变更 |
| 降级 | 无权限时从 TUN 自动切换到 no-tun 模式 |

---

**文档版本**:v1.0
**对应路线图**:Phase 0 起
**许可证**:Apache 2.0
**仓库**:https://github.com/LiStudioorg/limesh
