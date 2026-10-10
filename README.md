<div align="center">

# Limesh(立网)

**Lightweight mesh. Instantly.**

把分散在家庭、云、机房、容器里的机器,变成一个扁平的三层网络。

单二进制 · 30+ 平台 · 18 种传输协议 · 无 root 也能用 · 去中心化

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows%20%7C%20bsd-lightgrey.svg)](#平台支持)
[![Protocols](https://img.shields.io/badge/transports-18-brightgreen.svg)](AGENTS.md#9-传输协议18-种)
[![CGO](https://img.shields.io/badge/CGO-disabled-success.svg)](#从源码编译)
[![Website](https://img.shields.io/badge/website-limesh.dev-orange.svg)](https://limesh.dev)

[特性](#特性) · [快速开始](#快速开始) · [使用场景](#使用场景) · [核心概念](#核心概念) · [命令行](#命令行) · [配置](#配置) · [部署](#部署) · [FAQ](#常见问题faq) · [技术方案](AGENTS.md) · [配置参考](docs/CONFIG.md) · [传输协议](docs/TRANSPORT.md)

</div>

---

## 特性

| 特性 | 说明 |
|---|---|
| **单二进制,所有角色** | 一个 `mesh` 文件同时包含节点、中继、控制面、网关、出口节点。不需要装三个组件、不需要对齐版本 |
| **30+ 平台** | Linux(含 armv5~v7、mips、riscv64、loong64、s390x)、macOS、Windows、FreeBSD、OpenBSD、NetBSD、DragonFly、Solaris、Illumos、AIX |
| **18 种传输协议** | UDP、TCP、QUIC、TLS、WS、WSS、HTTP/2、HTTP/3、gRPC、KCP、DTLS、WireGuard、SSH、ICMP、DNS、Unix Socket、VSOCK、共享内存 |
| **无 root 也能用** | 没有 TUN 权限时自动降级到 gVisor 用户态栈 + SOCKS5 + 端口转发,不退出、不报错 |
| **去中心化** | 控制面只负责配置分发,不参与数据转发;控制面离线不影响已建立的连接 |
| **E2EE 端到端加密** | Noise IK 握手 + ChaCha20-Poly1305;中继转发密文,中继看不到明文 |
| **智能路由** | OSPF 子集自动收敛;`shortest` / `latency` / `weighted` 三种策略;EWMA 平滑抑制路径抖动 |
| **并发打洞** | STUN、ICE、UPnP、NAT-PMP、PCP、端口预测、生日悖论同时发起,首个成功者胜出 |
| **可观测** | Prometheus 指标、拓扑 API、结构化日志、审计日志 |
| **四层配置合并** | CLI > ENV > `[cli]` 块 > 文件 > 默认值,支持通配符与 include,可追踪每个值的来源 |
| **热重载** | fsnotify / SIGHUP / Admin API 三种触发;逐模块 Apply,失败自动回滚 |
| **纯 Go** | `CGO_ENABLED=0`,静态链接,scratch 镜像约 20 MB |

---

## 快速开始

### 安装

#### 一键脚本(Linux / macOS)

```bash
curl -fsSL https://limesh.dev/install.sh | sh
```

输出示例:

```
检测平台      linux/amd64
下载版本      v0.1.0
安装路径      /usr/local/bin/mesh
校验          sha256 OK
完成          mesh version → 0.1.0 (linux/amd64)
```

#### Docker

```bash
docker pull limesh/mesh:latest
docker run --rm limesh/mesh version
```

输出示例:

```
0.1.0 (linux/amd64, go1.22.5, CGO_ENABLED=0)
```

#### 手动下载

```bash
# 从 Releases 下载对应平台的压缩包
curl -fLO https://github.com/LiStudioorg/limesh/releases/latest/download/mesh_0.1.0_linux_amd64.tar.gz
tar -xzf mesh_0.1.0_linux_amd64.tar.gz
sudo install -m 0755 mesh /usr/local/bin/mesh
mesh version
```

#### Homebrew(macOS / Linux)

```bash
brew install limesh/tap/mesh
mesh version
```

#### Windows(PowerShell)

```powershell
Invoke-WebRequest -Uri "https://github.com/LiStudioorg/limesh/releases/latest/download/mesh_0.1.0_windows_amd64.zip" -OutFile mesh.zip
Expand-Archive mesh.zip -DestinationPath $env:ProgramFiles\mesh -Force
$env:Path += ";$env:ProgramFiles\mesh"
mesh version
```

#### 从源码编译

```bash
git clone https://github.com/LiStudioorg/limesh.git
cd limesh
CGO_ENABLED=0 go build -trimpath -o bin/mesh ./cmd/mesh
./bin/mesh version
```

要求 Go 1.22 或更高版本。`CGO_ENABLED=0` 是硬性要求,项目所有依赖均为纯 Go。

#### 路由器(OpenWrt)

```sh
# 在路由器上执行(架构通常是 mips / mipsle / armv7)
opkg update
opkg install curl
curl -fsSL https://limesh.dev/install.sh | sh -s -- --os linux --arch mipsle
mesh version
```

---

### 第一个网络(3 分钟)

目标:三台机器组网。**A 作为中继**,B 和 C 通过 A 加入同一个虚拟网络,并能互相 `ping` 通。

拓扑:

```
        ┌─────────────────┐
        │   A (公网 VPS)   │  relay   10.126.126.1
        │   1.2.3.4:11010 │
        └────┬───────┬────┘
             │       │
      ┌──────┘       └──────┐
      │                     │
┌─────┴──────┐       ┌──────┴─────┐
│ B (家里)    │       │ C (公司)    │
│ 10.126.126.x│       │ 10.126.126.y│
└────────────┘       └─────────────┘
```

#### 步骤 1:在 A 上启动中继

```bash
mesh relay --port 11010
```

输出示例:

```
2026-10-06T20:31:02+08:00  INFO  relay        starting
2026-10-06T20:31:02+08:00  INFO  relay        listening  udp/[::]:11010
2026-10-06T20:31:02+08:00  INFO  relay        listening  tcp/[::]:11010
2026-10-06T20:31:02+08:00  INFO  relay        node_id      a1b2c3d4e5f60718
2026-10-06T20:31:02+08:00  INFO  relay        privileges   none (this is expected)
2026-10-06T20:31:02+08:00  INFO  relay        ready
```

中继**不需要任何权限**,普通用户即可运行。确认防火墙放行:

```bash
# 云厂商安全组放行 11010/udp 与 11010/tcp
sudo ufw allow 11010/udp
sudo ufw allow 11010/tcp
```

#### 步骤 2:在 B 上启动节点

```bash
sudo mesh node \
  --network      prod \
  --secret       "change-me-to-a-strong-secret" \
  --relay        1.2.3.4:11010 \
  --hostname     node-b
```

输出示例:

```
2026-10-06T20:31:20+08:00  INFO  node    starting
2026-10-06T20:31:20+08:00  INFO  config  merged 3 sources (cli, file, default)
2026-10-06T20:31:20+08:00  INFO  identity  node_id     7f3e9a0c41b25d88
2026-10-06T20:31:20+08:00  INFO  ingress   tun         mesh0 up
2026-10-06T20:31:20+08:00  INFO  ingress   virtual_ip  10.126.126.42
2026-10-06T20:31:20+08:00  INFO  transport udp         listening :11010
2026-10-06T20:31:20+08:00  INFO  peer      relay       connected 1.2.3.4:11010 rtt=12ms
2026-10-06T20:31:20+08:00  INFO  mesh      joined      network=prod
2026-10-06T20:31:21+08:00  INFO  node      ready
```

#### 步骤 3:在 C 上启动节点

```bash
sudo mesh node \
  --network      prod \
  --secret       "change-me-to-a-strong-secret" \
  --relay        1.2.3.4:11010 \
  --hostname     node-c
```

输出示例:

```
2026-10-06T20:31:25+08:00  INFO  identity  node_id     5c1d8f2ab9034e77
2026-10-06T20:31:25+08:00  INFO  ingress   virtual_ip  10.126.126.87
2026-10-06T20:31:26+08:00  INFO  mesh      joined      network=prod
2026-10-06T20:31:27+08:00  INFO  peer      7f3e9a0c41b25d88  direct 10.126.126.42 rtt=8ms
2026-10-06T20:31:27+08:00  INFO  route     converged   2 peers, 1 direct, 1 relayed
```

> `secret` 必须三台机器完全一致。它同时是网络的准入凭证和 E2EE 密钥派生材料。

#### 步骤 4:验证连通性

在 B 上:

```bash
ping -c 3 10.126.126.87
```

```
PING 10.126.126.87 (10.126.126.87) 56(84) bytes of data.
64 bytes from 10.126.126.87: icmp_seq=1 ttl=64 time=8.42 ms
64 bytes from 10.126.126.87: icmp_seq=2 ttl=64 time=8.71 ms
64 bytes from 10.126.126.87: icmp_seq=3 ttl=64 time=8.55 ms
```

查看状态:

```bash
mesh status
```

```
Network      prod
NodeID       7f3e9a0c41b25d88
Hostname     node-b
VirtualIP    10.126.126.42/24
Ingress      tun (mesh0, mtu 1380)
Uptime       00:02:14
Peers        2 total
             1 direct   1 relayed
Routes       2
Relay        1.2.3.4:11010
```

查看对端:

```bash
mesh peer list
```

```
NODE ID            HOSTNAME   VIRTUAL IP       PATH     RTT    LOSS   STATE
5c1d8f2ab9034e77   node-c     10.126.126.87    direct   8.4ms  0.0%   active
a1b2c3d4e5f60718   relay-a    —                relayed  12ms   0.0%   active
```

#### 步骤 5:排错

```bash
# 1. 看详细日志
sudo journalctl -u mesh -f
# 或前台调试
sudo mesh node -c /etc/mesh/config.toml --log debug

# 2. 看拓扑(哪些节点连通、路径类型)
mesh topo
```

```
node-b (10.126.126.42)
├── node-c (10.126.126.87)      direct   rtt=8.4ms   ewma=8.6ms
└── relay-a (1.2.3.4:11010)     relayed  rtt=12.1ms  ewma=12.4ms
```

```bash
# 3. 检查配置来源(某个值为什么不是我设的?)
mesh config show --sources
```

```
network.secret    ← --secret (命令行)
network.subnet    ← /etc/mesh/config.toml:7
network.mtu       ← MESH_NETWORK_MTU
log.level         ← 默认值
```

```bash
# 4. 强制触发一次重连与打洞
mesh peer reconnect 5c1d8f2ab9034e77
```

```
reconnecting 5c1d8f2ab9034e77 ...
punch: 6 candidates, 1 succeeded (udp)
state: relayed → direct (8.4ms)
```

**常见失败原因**:

| 现象 | 原因 | 处理 |
|---|---|---|
| 日志出现 `secret mismatch` | 三台机器 `secret` 不一致 | 统一 `secret` 后重启 |
| 长时间停在 `relayed` | 打洞失败(对称 NAT / 防火墙) | 属于正常降级,功能可用;见 [打洞失败怎么办](#打洞失败怎么办) |
| `ingress tun: permission denied` | 缺少 CAP_NET_ADMIN | `setcap` 或改用 `--ingress no-tun` |
| 完全连不上 relay | 云安全组未放行 11010 | 同时放行 UDP 与 TCP |
| `ping` 不通但 `mesh peer list` 正常 | 本地防火墙拦截 TUN 接口 | `sudo iptables -I INPUT -i mesh0 -j ACCEPT` |

---

## 使用场景

### 场景 1:家庭组网

把家里的 NAS、软路由、树莓派、笔记本连成一个网段,出门在外也能直连访问。

```bash
# 在 NAS 上(有 root)
sudo mesh node --network home --secret "home-secret-2026" --hostname nas

# 在软路由上(OpenWrt)
mesh node --network home --secret "home-secret-2026" --hostname router \
  --subnet-proxy 192.168.1.0/24

# 在笔记本上(无 root,自动降级)
mesh node --network home --secret "home-secret-2026" --hostname laptop \
  --ingress auto
```

`--subnet-proxy 192.168.1.0/24` 表示:把路由器背后的整个物理局域网宣告进虚拟网络。此后在任何节点上都能直接访问 `192.168.1.50`(NAS 的物理 IP),不需要在每个设备上装 Limesh。

验证:

```bash
ping -c 2 192.168.1.50
curl -I http://192.168.1.50:5000
```

### 场景 2:云服务器互联

多台云服务器跨可用区、跨厂商互联,替代昂贵的专线与 VPC 对等连接。

```bash
# 阿里云 1.2.3.4
sudo mesh node --network prod --secret "$MESH_SECRET" --hostname aliyun-1

# 腾讯云 5.6.7.8
sudo mesh node --network prod --secret "$MESH_SECRET" --hostname tencent-1

# AWS 9.10.11.12
sudo mesh node --network prod --secret "$MESH_SECRET" --hostname aws-1
```

验证延迟并观察路径类型:

```bash
mesh peer list
```

```
NODE ID            HOSTNAME    VIRTUAL IP       PATH     RTT     LOSS
b21c0f4e88a13d90   aliyun-1    10.126.126.19    direct   2.1ms   0.0%
d47a91bb02ef5512   tencent-1   10.126.126.63    direct   24.8ms  0.0%
```

云厂商安全组需放行:

```
入方向  UDP 11010  来源 0.0.0.0/0
入方向  TCP 11010  来源 0.0.0.0/0
```

### 场景 3:K8s 集群

以 DaemonSet 部署,让每个 Node 加入虚拟网络;跨集群、跨云、本地 IDC 的 Pod 与 Node 可直接互访。

```bash
kubectl apply -f https://limesh.dev/deploy/k8s/daemonset.yaml
kubectl -n kube-system get pods -l app=mesh-node
```

```
NAME              READY   STATUS    RESTARTS   AGE
mesh-node-2xk9p   1/1     Running   0          42s
mesh-node-7d4mz   1/1     Running   0          42s
mesh-node-q8w1n   1/1     Running   0          42s
```

### 场景 4:跨地域办公

总部、分支、居家办公三种环境组成一个平面网络,访问内网服务不需要 VPN 客户端配置。

```bash
# 总部网关
sudo mesh gateway --network corp --secret "$MESH_SECRET" \
  --subnet-proxy 10.10.0.0/16

# 分支节点
sudo mesh node --network corp --secret "$MESH_SECRET" --hostname branch-sh

# 居家笔记本
mesh node --network corp --secret "$MESH_SECRET" --hostname wfh-01 --ingress no-tun --socks5 127.0.0.1:1080
```

居家环境用 SOCKS5 访问内网:

```bash
curl --socks5 127.0.0.1:1080 http://10.10.20.30:8080/health
```

### 场景 5:无 root 环境(共享服务器 / CI)

公司共享开发机、CI runner、受限容器里没有 root,也没有 `/dev/net/tun`。

```bash
mesh node --network dev --secret "$MESH_SECRET" --ingress no-tun
```

输出示例:

```
INFO  ingress  tun  unavailable: /dev/net/tun: no such file or directory
INFO  ingress  fallback   auto → no-tun (gVisor netstack)
INFO  ingress  no-tun  socks5 listening 127.0.0.1:1080
INFO  ingress  no-tun  forward tcp 127.0.0.1:15432 → 10.126.126.42:5432
INFO  node     ready   (limited mode: SOCKS5 + port forward only)
```

然后通过 SOCKS5 或端口转发访问对端服务:

```bash
# 方式一:SOCKS5
curl --socks5 127.0.0.1:1080 http://10.126.126.42:8000/

# 方式二:端口转发(本地 15432 → 对端 5432)
psql -h 127.0.0.1 -p 15432 -U app mydb
```

CI 中作为一次性节点:

```bash
mesh node --network ci --secret "$MESH_SECRET" --ingress no-tun \
  --forward tcp:127.0.0.1:15432=10.126.126.42:5432 &
mesh status --wait 10s
psql -h 127.0.0.1 -p 15432 -U app mydb -c "select 1"
```

---

## 核心概念

### 节点 / 中继 / 控制面

| 角色 | 做什么 | 需要 root | 挂了会怎样 |
|---|---|---|---|
| **节点** `node` | 加入网络,收发 IP 包 | 按 Ingress 模式 | 该机器掉线,其余不受影响 |
| **中继** `relay` | 为无法直连的节点转发密文 | 否 | 中继路径中断,直连路径不受影响 |
| **控制面** `controller` | 分发配置、同步拓扑、下发 ACL | 否 | 新节点无法加入,已建连接继续工作 |

### 虚拟网络 / NodeID / VirtualIP

```
NodeID      = SHA256(节点公钥)[:16]        ← 身份,永不变
VirtualIP   = 由 NodeID 确定性派生          ← 地址,全网无冲突、无需 DHCP
虚拟网段     = 10.126.126.0/24              ← 默认值,可配置
```

关键性质:**换网络、换中继、换 IP 都不改变 NodeID**,因而也不改变 VirtualIP。删掉一台机器再重装,只要密钥还在,它还是网络里的同一个节点。

```bash
mesh status --json | jq '{node_id, virtual_ip, hostname}'
```

```json
{
  "node_id": "7f3e9a0c41b25d88",
  "virtual_ip": "10.126.126.42",
  "hostname": "node-b"
}
```

### P2P 直连 vs 中继

```
直连(Direct)                     中继(Relayed)
A ═══════════════ B              A ──── relay ──── B
✓ 延迟最低                        ✓ 一定能通
✓ 不消耗中继带宽                  ✗ 多一跳,延迟增加
✗ 对称 NAT 下可能失败             ✗ 消耗中继带宽

策略:并发尝试所有打洞手段,成功即用直连;失败降级中继;
      中继状态下每 60 秒重试打洞,一旦成功自动切回直连。
```

### TUN vs No-TUN

| | TUN | No-TUN |
|---|---|---|
| 权限 | root 或 CAP_NET_ADMIN | **无需权限** |
| 透明性 | 全系统透明,任意应用可用 | 需应用支持 SOCKS5 或使用端口转发 |
| `ping` | 支持 | 不支持(用 TCP 探测) |
| 端口转发 | 支持 | 支持 |
| 适用 | 服务器、路由器、自有机器 | 共享主机、CI、受限容器 |

`--ingress auto`(默认)会自动选择:优先 TUN,失败则降级 No-TUN 并打印明确提示。

---

## 命令行

### 角色

#### `mesh node` — 节点

```bash
mesh node --network prod --secret "s3cr3t" --relay 1.2.3.4:11010 --hostname web-01
```

| Flag | 说明 | 默认 |
|---|---|---|
| `--network` | 网络名 | 必填 |
| `--secret` | 网络密钥(所有节点一致) | 必填 |
| `--hostname` | 节点主机名 | 系统主机名 |
| `--relay` | 中继地址,可重复 | 无 |
| `--ingress` | `tun` / `no-tun` / `auto` | `auto` |
| `--subnet` | 虚拟网段 | `10.126.126.0/24` |
| `--mtu` | 虚拟 MTU | `1380` |
| `--socks5` | no-tun 模式监听地址 | `127.0.0.1:1080` |
| `--forward` | 端口转发规则 | 无 |
| `--subnet-proxy` | 宣告物理子网 | 无 |
| `--log` | 日志级别 | `info` |

#### `mesh relay` — 中继

```bash
mesh relay --port 11010
```

零权限运行。输出示例:

```
INFO  relay  listening  udp/[::]:11010
INFO  relay  listening  tcp/[::]:11010
INFO  relay  node_id    a1b2c3d4e5f60718
INFO  relay  ready      (no elevated privileges required)
```

#### `mesh controller` — 控制面

```bash
mesh controller --listen 0.0.0.0:11011 --network prod
```

```
INFO  controller  listening   0.0.0.0:11011
INFO  controller  network     prod
INFO  controller  peers       0
INFO  controller  ready       (data plane not involved)
```

#### `mesh gateway` — 网关

```bash
sudo mesh gateway --network corp --secret "$MESH_SECRET" --subnet-proxy 10.10.0.0/16
```

#### `mesh exit` — 出口节点

```bash
sudo mesh exit --network prod --secret "$MESH_SECRET" --enable-forwarding
```

其他节点使用该出口:

```bash
mesh node --network prod --secret "$MESH_SECRET" --exit-node <exit-node-id>
```

### 工具

#### `mesh status` — 状态

```bash
mesh status
mesh status --json
mesh status --wait 10s        # 等待网络就绪,用于脚本
```

```bash
mesh status --json | jq .peers
```

```json
[
  { "node_id": "5c1d8f2ab9034e77", "virtual_ip": "10.126.126.87", "path": "direct", "rtt_ms": 8.4 }
]
```

#### `mesh peer` — 对端管理

```bash
mesh peer list                          # 列出对端
mesh peer show 5c1d8f2ab9034e77         # 查看单个对端详情
mesh peer add 9.9.9.9:11010             # 手动添加静态对端
mesh peer remove 9.9.9.9:11010          # 删除静态对端
mesh peer reconnect 5c1d8f2ab9034e77    # 强制重连(触发重新打洞)
```

```bash
mesh peer show 5c1d8f2ab9034e77
```

```
NodeID       5c1d8f2ab9034e77
Hostname     node-c
VirtualIP    10.126.126.87
Path         direct
Endpoint     203.0.113.9:44321
Transport    udp (quic available)
RTT          8.4ms   (ewma 8.6ms)
Loss         0.0%
Session      b41f92aa
Since        2026-10-06T20:31:27+08:00
```

#### `mesh route` — 路由管理

```bash
mesh route list
```

```
DESTINATION         NEXT HOP           METRIC   HOPS   PATH
10.126.126.87/32    10.126.126.87      8        1      direct
10.126.126.63/32    1.2.3.4:11010      34       2      relayed
192.168.1.0/24      10.126.126.19      12       2      direct
```

```bash
mesh route trace 10.126.126.87
```

```
traceroute to 10.126.126.87 (virtual)
 1  10.126.126.42 (self)      0.1ms
 2  10.126.126.87             8.4ms
path: direct (udp)
```

#### `mesh topo` — 拓扑

```bash
mesh topo
mesh topo --format dot | dot -Tpng -o topo.png
mesh topo --json | jq .
```

```
node-b (10.126.126.42)
├── node-c (10.126.126.87)     direct    rtt=8.4ms
├── relay-a (1.2.3.4)          relayed   rtt=12.1ms
└── 192.168.1.0/24             proxied via node-router
```

#### `mesh config` — 配置管理

```bash
mesh config show                    # 显示合并后的最终配置
mesh config show --sources          # 显示每一项的来源
mesh config validate -c a.toml      # 校验配置而不启动
mesh config diff -c new.toml        # 对比当前运行配置与文件差异
mesh config reload                  # 热重载
```

```bash
mesh config show --sources
```

```
network.name      ← --network (命令行)
network.secret    ← /etc/mesh/secrets/prod.toml:2
network.subnet    ← /etc/mesh/config.toml:7
network.mtu       ← MESH_NETWORK_MTU
log.level         ← 默认值
```

#### `mesh version` — 版本

```bash
mesh version
mesh version --json
```

```
0.1.0 (linux/amd64, go1.22.5, CGO_ENABLED=0, proto 1)
```

---

## 配置

### 配置文件位置

| 平台 | 默认路径 |
|---|---|
| Linux / macOS | `/etc/mesh/config.toml` |
| Windows | `%ProgramData%\mesh\config.toml` |
| 状态目录(所有平台) | `/var/lib/mesh/`(Windows 为 `%ProgramData%\mesh\state\`) |

状态目录保存:节点密钥、NodeID、会话缓存。**删除状态目录等于换一个身份**,请纳入备份。

### 最小配置示例

```toml
# /etc/mesh/config.toml
[network]
name     = "prod"
secret   = "change-me-to-a-strong-secret"
hostname = "node-a"
subnet   = "10.126.126.0/24"
mtu      = 1380

[ingress]
mode = "auto"

[relay]
servers = ["1.2.3.4:11010", "5.6.7.8:11010"]

[log]
level  = "info"
format = "text"
```

启动:

```bash
sudo mesh node -c /etc/mesh/config.toml
```

### 多文件 + 通配符

```bash
mesh node -c base.toml -c 'conf.d/*.toml'
mesh node -c 'conf.d/**/*.toml'      # 递归匹配
```

拆分配置的建议结构:

```
/etc/mesh/
├── config.toml          # 基础配置
├── conf.d/
│   ├── 10-network.toml
│   ├── 20-transport.toml
│   └── 30-observability.toml
└── secrets/
    └── prod.toml        # 必须 0600
```

```toml
# /etc/mesh/config.toml
include = ["conf.d/*.toml", "secrets/*.toml"]

[cli]
log = "info"
```

```toml
# /etc/mesh/secrets/prod.toml
[network]
secret = "super-strong-secret"
```

```bash
chmod 600 /etc/mesh/secrets/prod.toml
chmod 700 /etc/mesh/secrets
```

> `secrets/` 目录下的文件权限**必须**是 `0600`,否则 Limesh 拒绝启动。这是强制检查,不是警告。

### 优先级说明

```
默认值  <  文件字段  <  [cli] 块  <  环境变量  <  命令行
```

完整演示:

```toml
# /etc/mesh/config.toml
[log]
level = "warn"          # 第 2 层

[cli]
log = "debug"           # 第 3 层
```

```bash
# 第 4 层
MESH_LOG=error mesh config show --sources
```

```
log.level  ← MESH_LOG (环境变量)
```

```bash
# 第 5 层,最高优先级
MESH_LOG=error mesh node --log trace
```

```
log.level  ← --log (命令行)
```

变量名映射:`network.name` → `MESH_NETWORK_NAME`,即 `MESH_` + 路径大写 + 下划线。

### 热重载

三种触发方式:

```bash
# 1. 信号
sudo systemctl reload mesh          # 等价于 kill -HUP
sudo kill -HUP $(pidof mesh)

# 2. 命令行
mesh config reload

# 3. 文件监听(默认启用)
#    编辑 /etc/mesh/config.toml 保存即触发
```

热重载演示:

```bash
# 1. 查看当前值
mesh config show | grep -A2 '\[log\]'
```

```toml
[log]
level = "info"
```

```bash
# 2. 修改文件
sudo sed -i 's/level = "info"/level = "debug"/' /etc/mesh/config.toml

# 3. 观察日志(无需重启,断开连接数为 0)
sudo journalctl -u mesh -f
```

```
INFO  config  reload triggered   source=fsnotify file=/etc/mesh/config.toml
INFO  config  diff               log.level: info → debug        [hot]
INFO  config  applied            1 module, 0 failed, 0 pending-restart
```

模块响应级别:

| 级别 | 含义 | 例子 |
|---|---|---|
| `hot` | 原地生效,连接不中断 | 日志级别、路由策略 |
| `reconnect` | 重建传输,虚拟 IP 保留 | 监听端口、传输协议列表 |
| `restart` | 需要重启进程 | 网络密钥、NodeID 相关项 |

需要重启的变更不会被静默忽略:

```bash
mesh config diff
```

```
network.secret   changed    requires restart
```

### 完整配置参考

见 [docs/CONFIG.md](docs/CONFIG.md),包含全部配置项、类型、默认值与响应级别。

---

## 部署

### Docker

#### 节点(需要 TUN)

```bash
docker run -d --name mesh-node \
  --restart unless-stopped \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  -v /etc/mesh:/etc/mesh:ro \
  -v mesh-state:/var/lib/mesh \
  -p 11010:11010/udp \
  -p 11010:11010/tcp \
  limesh/mesh:latest node -c /etc/mesh/config.toml
```

#### 节点(无 TUN 权限)

```bash
docker run -d --name mesh-node \
  --restart unless-stopped \
  -v /etc/mesh:/etc/mesh:ro \
  -v mesh-state:/var/lib/mesh \
  -p 11010:11010/udp \
  limesh/mesh:latest node -c /etc/mesh/config.toml --ingress no-tun
```

#### 中继(零权限)

```bash
docker run -d --name mesh-relay \
  --restart unless-stopped \
  --network host \
  -v mesh-relay-state:/var/lib/mesh \
  limesh/mesh:latest relay --port 11010
```

> 中继建议使用 `--network host`。桥接网络会引入容器额外一层 NAT,降低打洞成功率并让中继看到的来源地址失真。

健康检查:

```bash
docker exec mesh-node /mesh status
```

### Docker Compose

```yaml
services:
  relay:
    image: limesh/mesh:latest
    container_name: mesh-relay
    restart: unless-stopped
    network_mode: host
    volumes:
      - relay-state:/var/lib/mesh
    command: ["relay", "--port", "11010"]

  node-a:
    image: limesh/mesh:latest
    container_name: mesh-node-a
    restart: unless-stopped
    depends_on: [relay]
    cap_add: [NET_ADMIN]
    devices: ["/dev/net/tun"]
    ports:
      - "11010:11010/udp"
    volumes:
      - ./config/node-a.toml:/etc/mesh/config.toml:ro
      - node-a-state:/var/lib/mesh
    command: ["node", "-c", "/etc/mesh/config.toml"]

volumes:
  relay-state:
  node-a-state:
```

```bash
docker compose up -d
docker compose exec node-a /mesh status
```

### Kubernetes

```bash
kubectl apply -f https://limesh.dev/deploy/k8s/daemonset.yaml
```

或使用 Helm(完整参数见 [AGENTS.md](AGENTS.md#16-k8s-部署)):

```bash
helm repo add limesh https://charts.limesh.dev
helm install mesh limesh/mesh \
  --namespace kube-system \
  --set network.name=prod \
  --set network.secret="$MESH_SECRET"
```

验证:

```bash
kubectl -n kube-system get ds mesh-node
kubectl -n kube-system logs -l app=mesh-node --tail=20
```

### systemd

```ini
# /etc/systemd/system/mesh.service
[Unit]
Description=Limesh Mesh Node
Documentation=https://limesh.dev
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/mesh node -c /etc/mesh/config.toml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW
NoNewPrivileges=false
ProtectSystem=strict
ReadWritePaths=/var/lib/mesh
StateDirectory=mesh
ConfigurationDirectory=mesh

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mesh
sudo systemctl status mesh
sudo journalctl -u mesh -f
```

一键安装脚本会同时完成二进制安装、systemd unit 写入与 capabilities 设置:

```bash
curl -fsSL https://limesh.dev/install.sh | sudo sh -s -- --install-systemd
```

---

## 权限

### 需要 root 吗?

**大多数情况不需要。** 只有创建 TUN 设备需要特权,其余全部功能在无 root 下可用。

- 中继:始终零权限。
- 控制面:始终零权限。
- 节点:仅 TUN 模式需要特权;无特权时自动降级到 no-tun 模式。
- 出口/网关:需要转发能力(通常等同 root 或 CAP_NET_ADMIN)。

### 各平台权限要求

| 平台 | node(TUN) | node(no-tun) | relay |
|---|---|---|---|
| Linux | `setcap cap_net_admin,cap_net_raw+ep` | 无需 | 无需 |
| macOS | `sudo` | 无需 | 无需 |
| Windows | 管理员 | 无需 | 无需 |
| Docker | `NET_ADMIN` + `/dev/net/tun` | 无需 | 无需 |
| K8s | `securityContext.capabilities` | 无需 | 无需 |
| OpenWrt | `setcap` 或 root | 无需 | 无需 |
| Android | 无需 root(系统 VPNService) | — | — |
| iOS | 无需 root(NetworkExtension) | — | — |

Linux 推荐用 capabilities 而非 root:

```bash
sudo setcap cap_net_admin,cap_net_raw+ep /usr/local/bin/mesh
getcap /usr/local/bin/mesh
```

```
/usr/local/bin/mesh cap_net_admin,cap_net_raw=ep
```

> 二进制升级后会丢失 capability(setcap 打在文件上)。安装脚本每次安装后都会重新执行 setcap;手动升级时请记得重做。

### 无 root 模式说明

无 root 时自动降级,启动日志会明确提示:

```
INFO  ingress  tun  unavailable: operation not permitted
INFO  ingress  fallback   auto → no-tun (gVisor netstack)
INFO  ingress  no-tun  socks5 listening 127.0.0.1:1080
WARN  ingress  limited mode: no transparent routing, use SOCKS5 or --forward
```

**能做**:

- 通过 SOCKS5(`127.0.0.1:1080`)访问虚拟网络内任意 TCP 服务。
- 通过 `--forward tcp:本地端口=虚拟IP:端口` 做端口转发。
- 作为服务端被其他节点访问(监听虚拟 IP 上的端口)。
- 参与路由、中继选择、拓扑上报。

**不能做**:

- 全局透明路由(不经过 SOCKS5 的流量不会走虚拟网络)。
- 直接 `ping` 虚拟 IP(`icmp` 需要 raw socket;可用 TCP 探测代替)。
- 子网代理(`--subnet-proxy` 需要配置本地路由表)。

显式指定 no-tun(避免在无权限环境里做无谓的 TUN 尝试):

```bash
mesh node --ingress no-tun --socks5 127.0.0.1:1080 \
  --forward tcp:127.0.0.1:15432=10.126.126.42:5432
```

---

## 平台支持

| 系统 | 架构 | 状态 |
|---|---|---|
| Linux | amd64, arm64, armv5, armv6, armv7, 386, mips, mipsle, mips64le, riscv64, loong64, ppc64le, s390x | 完整支持 |
| macOS | amd64, arm64 | 完整支持 |
| Windows | amd64, arm64, 386 | 完整支持 |
| FreeBSD | amd64, arm64 | 完整支持 |
| OpenBSD | amd64, arm64 | 完整支持 |
| NetBSD | amd64, arm64 | 完整支持 |
| DragonFly | amd64, arm64 | 完整支持 |
| Solaris | amd64 | 基础支持 |
| Illumos | amd64 | 基础支持 |
| AIX | ppc64 | 基础支持 |
| Android | arm64 | 后期(gomobile) |
| iOS | arm64 | 后期(gomobile) |

全部产物由 `CGO_ENABLED=0` 静态交叉编译,无 libc 依赖。查看当前机器可用的下载包:

```bash
mesh version
curl -s https://limesh.dev/api/releases/latest | jq '.assets[].name'
```

---

## 性能

以下为参考量级(单跳直连,千兆内网,MTU 1380,macOS arm64 ↔ Linux amd64 通过 1 Gbps 链路),实际结果随硬件、加密加速与路径变化:

| 指标 | 数值 | 条件 |
|---|---|---|
| 吞吐(TUN 模式) | ~1.6 Gbps | 单流 iperf3,ChaCha20-Poly1305 硬件加速 |
| 吞吐(TUN 模式) | ~3.2 Gbps | 4 并发流 |
| 吞吐(no-tun, gVisor) | ~400 Mbps | 用户态栈开销 |
| 吞吐(Unix Socket 传输) | ~8 Gbps | 同机通信,绕过网络栈 |
| 延迟增量 | +0.3 ~ 1.2 ms | 相对裸链路,单跳加密转发 |
| 延迟增量(中继) | +RTT(relay) | 中继引入一跳 |
| 建连时间(P2P 直连) | 150 ~ 600 ms | 含握手与打洞竞赛 |
| 建连时间(降级中继) | ≤ 5 s | 打洞超时后切换 |
| 内存占用 | ~45 MB RSS | 单节点,100 对端 |
| 内存占用 | ~18 MB RSS | 单节点,5 对端 |
| 二进制体积 | ~22 MB | 含全部 18 种传输,未压缩 |
| 单节点对端容量 | 1000+ | 受限于内存与文件描述符 |

带外并发上限与 `LimitNOFILE` 直接相关,生产环境建议:

```ini
LimitNOFILE=1048576
```

查看本机实时指标:

```bash
curl -s localhost:9100/metrics | grep '^mesh_'
```

```
mesh_peer_total 12
mesh_peer_direct 9
mesh_peer_relayed 3
mesh_packet_sent_total 1842391
mesh_bytes_encrypted_total 2417813402
mesh_route_convergence_seconds 0.42
```

---

## 架构

```
┌─────────────────────────────────────────────┐
│ L7 应用层   CLI | Web 面板 | API | 移动端    │
├─────────────────────────────────────────────┤
│ L6 控制面   配置 | 热重载 | 拓扑 | 密钥      │
├─────────────────────────────────────────────┤
│ L5 路由层   OSPF | 策略引擎 | Peer 状态机    │
├─────────────────────────────────────────────┤
│ L4 数据面   ZCPacket | 多路径 | QoS | 加密   │
├─────────────────────────────────────────────┤
│ L3 传输层   18 种传输协议                    │
├─────────────────────────────────────────────┤
│ L2 穿透层   STUN | ICE | UPnP | 打洞         │
├─────────────────────────────────────────────┤
│ L1 接入层   TUN | gVisor Netstack | SOCKS5   │
├─────────────────────────────────────────────┤
│ L0 观测层   Prometheus | 日志 | 审计         │
└─────────────────────────────────────────────┘
```

包描述、协议线格式、状态机、配置合并算法等完整设计见 [AGENTS.md](AGENTS.md)。

---

## 常见问题(FAQ)

### 为什么 `mesh peer add` 后连接不上?

当前版本(Phase 1a)需要**手工提供对端公钥**。Noise IK 握手要求发起方预先知道
响应方的长期公钥,而自动获取公钥需要控制面下发或发现机制——这部分将在 Phase 2 支持。

因此在 Phase 1a:

- 用 `mesh peer add` 只填了地址是不够的,还需要对端公钥;
- 对端公钥可用 `mesh status` 查看(或在 `PeerSpec{Addr, PublicKey}` 中显式注入);
- 完整流程(控制面自动下发公钥)将在 Phase 2 支持。

> 另:Phase 1a 尚未实现真实打洞与中继。若 `mesh peer list` 显示 `probing` 或
> `relayed`,说明状态机在推进,但底层仍是状态占位,不代表已建立真实传输路径。
> 详见 [AGENTS.md §8.9.3](</AGENTS.md>)。

### 打洞失败怎么办?

**先确认这是不是问题。** 打洞失败会自动降级到中继,网络依然可用,只是延迟略高、占用中继带宽。

排查顺序:

```bash
# 1. 看当前路径类型
mesh peer list
```

```
NODE ID            HOSTNAME   PATH      RTT
5c1d8f2ab9034e77   node-c     relayed   34.2ms
```

```bash
# 2. 看候选地址(是否只拿到内网地址?)
mesh peer show 5c1d8f2ab9034e77 --verbose
```

```
candidates:
  udp 192.168.1.42:11010      (local)
  udp 10.0.0.7:11010          (local)
  ← 没有公网候选地址,说明 STUN 未成功
```

常见原因与处理:

| 原因 | 判断方法 | 处理 |
|---|---|---|
| 双方都是对称 NAT | 两侧候选都只有内网地址 | 部署一个公网 relay,中继路径本就为此时设计 |
| 云安全组未放行 UDP | `nc -u -v 1.2.3.4 11010` 无响应 | 放行 `11010/udp`,不要只放 TCP |
| 防火墙拦出站 UDP | `mesh peer show --verbose` 无 STUN 结果 | 放行出站 UDP 到任意端口 |
| 容器桥接网络 | 候选地址是 `172.17.x.x` | 改用 `--network host` |
| 运营商级 NAT(CGNAT) | 公网候选端口与其他服务冲突 | 中继 + 端口预测,或使用 IPv6 |

**IPv6 优先**:双方都有公网 IPv6 时,Limesh 优先走 IPv6 直连。若长期只能中继,先检查是否真的没有 IPv6 连通性。

### 怎么选传输协议?

绝大多数情况**不用选**,默认自动评分即可。需要显式指定时:

```toml
[transport]
protocols = ["udp", "quic", "tcp"]
```

| 环境 | 推荐 |
|---|---|
| 公网直连,无特殊限制 | `udp`(默认首选) |
| UDP 被 QoS 或封锁 | `quic` 或 `kcp` |
| 只放行 HTTP/HTTPS | `ws` / `wss` / `tls` |
| 只放行 443 且需要伪装 | `wss` |
| 企业代理环境 | `http2` / `grpc` |
| 只开放 22 端口 | `ssh` |
| 只放行 ICMP | `icmp`(吞吐低,最后手段) |
| 只放行 53 端口 | `dns`(吞吐低,最后手段) |
| 同机多进程 | `unix`(最低延迟) |
| 虚拟机 ↔ 宿主 | `vsock` |
| 与现有 WireGuard 网络互通 | `wireguard` |

查看当前实际使用的传输:

```bash
mesh peer show 5c1d8f2ab9034e77 | grep Transport
```

```
Transport    udp (quic available, kcp available)
```

每种协议的实现要点、能力矩阵、性能量级与新增传输的方法,见 [docs/TRANSPORT.md](docs/TRANSPORT.md)。

### 中继怎么部署?

公网 VPS 一条命令即可,无需任何权限:

```bash
mesh relay --port 11010
```

建议:

```bash
# 多中继(节点侧配置,就近选择)
mesh node --network prod --secret "$S" --relay 1.2.3.4:11010 --relay 5.6.7.8:11010
```

- 用 `--network host`(Docker)避免容器 NAT 干扰。
- 至少部署 2 个中继,分布在不同地域与不同运营商。
- 中继带宽按"最坏情况全部走中继"估算上限,配合 `[relay].quota` 限制。
- 中继看不到明文,但可以看到流量大小与通信双方,敏感场景请自建中继。

检查中继负载:

```bash
curl -s http://1.2.3.4:9100/metrics | grep mesh_relay_
```

```
mesh_relay_sessions 42
mesh_relay_bytes_forwarded_total 18472910334
mesh_relay_active_peers 87
```

### 和 Tailscale / EasyTier 有什么区别?

| 维度 | Limesh | Tailscale | EasyTier | ZeroTier |
|---|---|---|---|---|
| 控制面 | 自托管 controller 或去中心化 | 官方协调服务器(可自建 Headscale) | 去中心化 | 官方根服务器 |
| 传输协议 | 18 种,可插拔 | WireGuard 单协议 | 3~4 种 | 自有 UDP |
| 无 root | 一等公民(no-tun + SOCKS5) | 部分受限 | 需 TUN | 部分受限 |
| 中继权限 | 零权限 | 无需 root | 无需 root | 无需 root |
| 配置系统 | TOML 四层合并 + 来源追踪 + 热重载 | 以控制面为准 | 命令行/文件 | 控制台 |
| 路由 | OSPF 子集 + 三种策略 + EWMA | DERP 中继 + 直连 | 直连优先 | 自有 |
| 平台数 | 30+ 组合 | 主流平台 | 主流平台 | 主流平台 |
| 许可证 | Apache 2.0 | BSD-3 | LGPL-3.0 | BUSL |

选择建议:已经有 Tailscale 生态用 Tailscale;需要在极度受限环境(无 root、只放行特定端口、需要伪装流量)部署,或者需要自托管的可观测与可运营能力,选 Limesh。

### 无 root 能做什么?

见 [无 root 模式说明](#无 root-模式说明)。一句话:**SOCKS5 与端口转发可用,全局透明路由不可用**。

```bash
mesh node --network dev --secret "$S" --ingress no-tun \
  --socks5 127.0.0.1:1080 \
  --forward tcp:127.0.0.1:15432=10.126.126.42:5432
```

### 支持 IPv6 吗?

支持,并且在双栈环境下**优先使用 IPv6**。IPv6 通常没有 NAT,直连成功率显著高于 IPv4。

```bash
# 只使用 IPv6
mesh node --network prod --secret "$S" --ipv6-only

# 查看候选地址族
mesh peer show <node-id> --verbose | grep candidates
```

```
candidates:
  udp6 [2408:xxxx::1]:11010   (public, preferred)
  udp4 203.0.113.9:11010      (public)
```

### 配置改了没生效?

九成是优先级问题:

```bash
mesh config show --sources
```

```
log.level  ← --log (命令行)     ← 命令行覆盖了文件里的设置
```

### 删除节点后网络里还有僵尸节点?

多半是删掉了状态目录导致 NodeID 变化,旧身份仍在 LSA 中直到 `LSAMaxAge`(300 秒)超时。保留 `/var/lib/mesh/` 即可避免。

---

## 路线图

| 阶段 | 周期 | 内容 |
|---|---|---|
| Phase 0 | 2 周 | 骨架、ZCPacket、UDP 传输、Config、单二进制、两节点互通 |
| Phase 1 | 6 周 | Noise 握手、OSPF 路由、Peer 状态机、TUN、完整 CLI |
| Phase 2 | 6 周 | STUN/ICE、中继、UPnP、端口预测、QUIC/TCP/TLS/WS/WSS/KCP |
| Phase 3 | 4 周 | gVisor 用户态、SOCKS5、Prometheus、拓扑 API |
| Phase 4 | 4 周 | E2EE、ACL、密钥轮换、Docker、K8s、WireGuard 互操作 |
| Phase 5 | 4 周 | 延迟优先路由、多路径、出口节点、MagicDNS、Web 面板 |
| Phase 6 | 后期 | SSH/ICMP/DNS 隧道、Unix Socket/VSOCK、gomobile、GUI |

详细出口标准见 [AGENTS.md](AGENTS.md#18-路线图)。

---

## 贡献

欢迎一切形式的贡献:代码、文档、报 bug、提需求、翻译、真机测试冷门平台。

### 报 Bug

请提供:

1. `mesh version` 输出。
2. `mesh status --json` 与 `mesh peer list` 输出(隐去密钥与真实 IP)。
3. 复现步骤与预期/实际行为。
4. `--log debug` 下的相关日志片段。

### 提需求

先在 GitHub Discussions 讨论场景与动机,再开 Issue。需求描述请包含:要解决什么问题、当前如何绕过、期望的配置形态。

### 提交代码

```bash
git clone https://github.com/LiStudioorg/limesh.git
cd limesh
git checkout -b feat/my-change

# 开发
make build
make test
make lint

# 提交(需 DCO 签名)
git commit -s -m "feat(transport): add wireguard interop"
git push origin feat/my-change
```

PR 要求:

- `go test ./... -race` 通过。
- `CGO_ENABLED=0` 构建通过。
- 遵循 [AGENTS.md](AGENTS.md#3-核心设计原则) 中的八条设计原则。
- 新增依赖需说明替代方案、体积影响与许可证兼容性。
- 提交信息遵循 Conventional Commits。

开发环境要求:

```bash
go version      # >= 1.22
make dev        # 安装开发工具(staticcheck、golangci-lint 等)
```

### 文档

文档改动同样走 PR 流程。四份文档各有分工:

| 文档 | 面向 | 内容 |
|---|---|---|
| [README.md](README.md) | 用户 | 安装、快速开始、场景、命令行、排错 |
| [docs/CONFIG.md](docs/CONFIG.md) | 用户与运维 | 全部配置项、类型、默认值、优先级、热重载级别 |
| [docs/TRANSPORT.md](docs/TRANSPORT.md) | 开发者 | 18 种传输详解、选型决策树、能力矩阵、性能量级、扩展指南 |
| [AGENTS.md](AGENTS.md) | 开发者与贡献者 | 架构分层、协议规范、设计原则、路线图 |

深入阅读建议:

- 想调整配置但不确定某项的含义或生效范围 → [docs/CONFIG.md](docs/CONFIG.md)
- 网络环境受限,需要选一种能穿透的传输 → [docs/TRANSPORT.md](docs/TRANSPORT.md) 的[选型决策树](docs/TRANSPORT.md#5-选型决策树)
- 想给 Limesh 加一种新传输 → [docs/TRANSPORT.md](docs/TRANSPORT.md) 的[扩展指南](docs/TRANSPORT.md#8-扩展指南)
- 想理解某项设计为什么这么做 → [AGENTS.md](AGENTS.md) 与附录 A 的设计决策速查

---

## 社区

- **GitHub Discussions**:https://github.com/LiStudioorg/limesh/discussions —— 提问、场景讨论、经验分享
- **GitHub Issues**:https://github.com/LiStudioorg/limesh/issues —— bug 与功能需求
- **Discord**:https://limesh.dev/discord —— 实时交流
- **微信群**:添加 `limesh-dev` 并备注 "limesh",由维护者拉入
- **官网**:https://limesh.dev

提问前请先搜索已有讨论,并附上 `mesh version` 与相关日志。

---

## 许可证

Apache License 2.0,详见 [LICENSE](LICENSE)。

```
Copyright 2026 LiStudio

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
```

---

## 致谢

Limesh 站在这些优秀项目之上:

| 项目 | 用途 |
|---|---|
| [wireguard-go](https://git.zx2c4.com/wireguard-go) | TUN 设备抽象与 Noise 协议实现参考 |
| [gVisor](https://gvisor.dev) | 用户态网络栈,支撑无 root 模式 |
| [quic-go](https://github.com/quic-go/quic-go) | QUIC 与 HTTP/3 传输 |
| [kcp-go](https://github.com/xtaci/kcp-go) | KCP 可靠 UDP 传输 |
| [pion/stun](https://github.com/pion/stun) · [pion/ice](https://github.com/pion/ice) | NAT 类型探测与 ICE 打洞 |
| [koanf](https://github.com/knadh/koanf) | 多源配置合并与来源追踪 |
| [cobra](https://github.com/spf13/cobra) · [pflag](https://github.com/spf13/pflag) | 命令行框架 |
| [zap](https://github.com/uber-go/zap) | 结构化日志 |
| [client_golang](https://github.com/prometheus/client_golang) | Prometheus 指标 |

同时感谢 EasyTier、Tailscale、NetBird、ZeroTier 在组网领域做出的探索。

<div align="center">

**Lightweight mesh. Instantly.**

[⬆ 回到顶部](#limesh立网)

</div>
