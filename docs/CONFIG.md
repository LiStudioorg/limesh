# Limesh 配置参考

> 本文档是 Limesh 的配置参考手册,面向需要精细调整配置的用户与运维人员。
>
> 技术方案总纲见 [AGENTS.md](../AGENTS.md),用户入门见 [README.md](../README.md),传输协议详解见 [TRANSPORT.md](TRANSPORT.md)。

---

## 目录

1. [快速开始](#1-快速开始)
2. [配置文件位置与格式](#2-配置文件位置与格式)
3. [优先级](#3-优先级)
4. [多文件加载](#4-多文件加载)
5. [内嵌 \[cli\] 块](#5-内嵌-cli-块)
6. [配置项参考](#6-配置项参考)
7. [热重载](#7-热重载)
8. [安全](#8-安全)
9. [配置管理命令](#9-配置管理命令)
10. [完整示例](#10-完整示例)
11. [故障排查](#11-故障排查)

---

## 1. 快速开始

最小可用配置:

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

[log]
level  = "info"
format = "text"
```

启动与验证:

```bash
sudo mesh node -c /etc/mesh/config.toml

# 另开终端确认实际生效的配置
mesh config show
```

```
network.name       prod
network.hostname   node-a
network.subnet     10.126.126.0/24
network.mtu        1380
ingress.mode       auto
log.level          info
```

必填项只有两个:`network.name` 与 `network.secret`。其余全部有默认值。

> `network.secret` 必须在同一网络的所有节点上完全一致。它同时是网络的准入凭证与 E2EE 密钥派生材料,不一致会导致握手失败。

---

## 2. 配置文件位置与格式

| 平台 | 默认路径 |
|---|---|
| Linux / macOS | `/etc/mesh/config.toml` |
| Windows | `%ProgramData%\mesh\config.toml` |
| 状态目录 | `/var/lib/mesh/`(Windows 为 `%ProgramData%\mesh\state\`) |

**格式仅支持 TOML。** 不提供 YAML、JSON、INI 等其他格式。

理由:单一格式意味着单一的合并语义。多格式并存会在"数组是追加还是替换""null 与缺失是否等价"这类问题上产生无法收敛的分歧,而这些分歧最终都会变成难以复现的线上问题。

显式指定配置文件:

```bash
mesh node -c /etc/mesh/config.toml
```

不指定 `-c` 时,按上述默认路径查找;默认路径不存在则使用纯默认值启动(此时 `network.name` 与 `network.secret` 必须由 CLI 或 ENV 提供)。

状态目录与配置文件是两回事:

| | 内容 | 丢失后果 |
|---|---|---|
| 配置文件 | 网络名、密钥、对端、策略 | 可用默认值重启,但需重新提供密钥 |
| 状态目录 | 节点密钥、NodeID、会话缓存 | **节点身份改变**,虚拟 IP 随之改变 |

请把 `/var/lib/mesh/` 纳入备份。删除它等价于换一台新机器入网。

---

## 3. 优先级

合并顺序,后者覆盖前者:

```
默认值(Default) < 文件字段(TOML) < 内嵌 [cli] 块 < 环境变量(ENV) < 命令行(CLI)
```

注意 `[cli]` 块的位置:**位于默认值与环境变量之间**。写进配置文件的 CLI 语法会被环境变量和命令行覆盖,但会覆盖文件中的普通字段。

### 3.1 五层完整演示

同一项 `log.level`,逐层覆盖:

**第 1 层 · 默认值**

```bash
mesh config show --sources | grep log.level
```

```
log.level  ← 默认值        (info)
```

**第 2 层 · 文件字段**

```toml
# /etc/mesh/config.toml
[log]
level = "warn"
```

```bash
mesh config show --sources | grep log.level
```

```
log.level  ← /etc/mesh/config.toml:2
```

**第 3 层 · 内嵌 `[cli]` 块**

```toml
# /etc/mesh/config.toml
[log]
level = "warn"

[cli]
log = "debug"
```

```bash
mesh config show --sources | grep log.level
```

```
log.level  ← [cli] 块 (/etc/mesh/config.toml:5)
```

**第 4 层 · 环境变量**

```bash
MESH_LOG=error mesh config show --sources | grep log.level
```

```
log.level  ← MESH_LOG
```

**第 5 层 · 命令行**

```bash
MESH_LOG=error mesh node --log trace
```

```
log.level  ← --log (命令行)
```

### 3.2 环境变量命名规则

配置路径 → 环境变量名的映射规则:

```
MESH_ + 路径各段大写 + 用下划线连接
```

| 配置项 | 环境变量 |
|---|---|
| `network.name` | `MESH_NETWORK_NAME` |
| `network.secret` | `MESH_NETWORK_SECRET` |
| `network.mtu` | `MESH_NETWORK_MTU` |
| `ingress.mode` | `MESH_INGRESS_MODE` |
| `log.level` | `MESH_LOG_LEVEL` |
| `log.level` 简写 | `MESH_LOG` |
| `routing.strategy` | `MESH_ROUTING_STRATEGY` |
| `observability.metrics.enabled` | `MESH_OBSERVABILITY_METRICS_ENABLED` |

布尔值接受 `true` / `false` / `1` / `0`;数组用逗号分隔:

```bash
export MESH_NETWORK_NAME=prod
export MESH_LISTEN_PORT=11010
export MESH_OBSERVABILITY_METRICS_ENABLED=true
```

### 3.3 优先级冲突排查

任何时候不确定某个值从哪来:

```bash
mesh config show --sources
```

```
network.name      ← --network (命令行)
network.secret    ← /etc/mesh/secrets/prod.toml:2
network.subnet    ← /etc/mesh/config.toml:7
network.mtu       ← MESH_NETWORK_MTU
log.level         ← [cli] 块 (/etc/mesh/config.toml:11)
routing.strategy  ← 默认值
```

绝大多数"配置没生效"的问题,答案都是**它被更高优先级的来源覆盖了**。

---

## 4. 多文件加载

`-c` 可以重复出现,按给出顺序依次合并:

```bash
mesh node -c base.toml -c override.toml
```

### 4.1 通配符

```bash
# 匹配单层
mesh node -c 'conf.d/*.toml'

# 递归匹配多层
mesh node -c 'conf.d/**/*.toml'

# 混合:先基础文件,再目录内全部
mesh node -c base.toml -c 'conf.d/**/*.toml'
```

规则:

| 规则 | 说明 |
|---|---|
| `*` | 匹配单层路径中的任意文件名 |
| `**` | 递归匹配任意层级 |
| 排序 | 展开结果按**字典序**排序后合并 |
| 无匹配 | 通配符无匹配时**不报错**,视为空(显式文件名不存在则报错) |
| 引号 | 通配符必须加引号,否则由 shell 提前展开,导致不同机器上顺序不一致 |

字典序排序 + 数字前缀命名,可以让合并顺序可预测:

```
/etc/mesh/conf.d/
├── 10-network.toml
├── 20-transport.toml
├── 30-observability.toml
└── 99-override.toml
```

### 4.2 include

配置文件内部可以引用其它文件:

```toml
# /etc/mesh/config.toml
include = ["conf.d/*.toml", "secrets/*.toml"]

[network]
name = "prod"
```

被引用的文件本身还可以继续 include,形成加载树。

| 规则 | 说明 |
|---|---|
| 路径基准 | 相对于**当前文件所在目录**,而非进程工作目录 |
| 排序 | 同通配符,按字典序 |
| 循环检测 | 检测到循环 include **立即报错退出**,不静默忽略 |
| 重复引用 | 同一文件按解析后的绝对路径去重,只生效一次 |
| 通配符 | include 的路径同样支持 `*` 与 `**` |

路径基准用"相对于当前文件"而非"相对于工作目录"是有意设计:配置文件因此可以被整体搬迁或版本化,不依赖启动时的工作目录。

推荐的目录结构:

```
/etc/mesh/
├── config.toml              # 主配置,含 include 与 [cli]
├── conf.d/
│   ├── 10-network.toml
│   ├── 20-ingress.toml
│   ├── 30-transports.toml
│   └── 40-observability.toml
└── secrets/
    └── prod.toml            # 必须 0600,只放密钥
```

```toml
# /etc/mesh/config.toml
include = ["conf.d/*.toml", "secrets/*.toml"]

[cli]
log = "info"
```

```toml
# /etc/mesh/conf.d/10-network.toml
[network]
name     = "prod"
hostname = "node-a"
subnet   = "10.126.126.0/24"
mtu      = 1380
```

```toml
# /etc/mesh/secrets/prod.toml
[network]
secret = "super-strong-secret"
```

```bash
chmod 700 /etc/mesh/secrets
chmod 600 /etc/mesh/secrets/prod.toml
```

### 4.3 合并规则

| 类型 | 规则 | 示例 |
|---|---|---|
| map(表) | **递归合并**,同键继续下钻 | 两个 `[network]` 表按字段合并 |
| 数组 | **追加**(前者在前,后者在后),不去重 | 两份 `peers.static` 合并为长列表 |
| 标量 | **覆盖**(高优先级胜出) | `mtu = 1380` 覆盖 `mtu = 1400` |

**map 递归合并**

```toml
# base.toml
[network]
name = "prod"
mtu  = 1380
```

```toml
# override.toml
[network]
mtu = 1400
```

结果:`name = "prod"`(保留),`mtu = 1400`(被覆盖)。未提及的字段不会被清空。

**数组追加**

```toml
# base.toml
[peers]
static = ["1.2.3.4:11010"]
```

```toml
# override.toml
[peers]
static = ["5.6.7.8:11010"]
```

结果:

```toml
[peers]
static = ["1.2.3.4:11010", "5.6.7.8:11010"]
```

数组追加是有意设计。对端列表、ACL 规则、传输协议候选这类配置,期望的是"基础配置 + 增量补充",而不是"后写的把前面的整体抹掉"。

**清空数组:必须用 `reset`,裸写 `[]` 不清空**

数组语义统一为"追加"后,`[]` 就失去了表达"清空"的能力——它只是"追加零个元素",等价于什么都不做:

```toml
# base.toml
[peers]
static = ["1.2.3.4:11010"]
```

```toml
# override.toml
[peers]
static = []          # ❌ 继承值仍在:["1.2.3.4:11010"]
```

要真正清空,使用顶层 `reset` 字段:

```toml
# override.toml
reset = ["peers"]    # ✅ 先丢弃继承来的 peers,后面的定义才是最终值

[peers]
static = ["5.6.7.8:11010"]
```

结果:

```toml
[peers]
static = ["5.6.7.8:11010"]
```

`reset` 只清空**低优先级来源**的值,不能越过 ENV 与 CLI:

```toml
# /etc/mesh/config.toml
reset = ["log"]
```

```bash
MESH_LOG=debug mesh node -c /etc/mesh/config.toml
```

结果:`log.level = "debug"`。`reset` 清掉了文件与 `[cli]` 块里的 `[log]`,但环境变量仍然生效。

> 空数组与"不写这个字段"语义也不同:**不写** = 不参与合并;**写 `[]`** = 追加零个元素,原有内容保留。两者都不会清空。
>
> `reset` 的完整语义(字段路径、多层并集、与优先级的交互)见 [AGENTS.md](../AGENTS.md#1041-reset显式清空继承来的值),对应配置项见 [6.0 顶层字段](#60-顶层字段)。

**标量覆盖**

```toml
# base.toml        # override.toml
[network]          [network]
mtu = 1380         mtu = 1400
```

结果:`mtu = 1400`。

---

## 5. 内嵌 [cli] 块

`[cli]` 块允许在配置文件里写 flag 语法,让"打算怎么跑"与"配置是什么"共处一地:

```toml
[cli]
log   = "info"
topo  = true
```

语义:

| 规则 | 说明 |
|---|---|
| 键名 | 与 flag 名**完全一致**,去掉 `--` 前缀 |
| 布尔 flag | 写 `true` 表示启用 |
| 数组 | 用 TOML 数组表示 |
| 优先级 | 高于文件字段与默认值,低于 ENV 与命令行 |

等价关系:

| `[cli]` 块 | 等价命令行 |
|---|---|
| `log = "debug"` | `--log debug` |
| `topo = true` | `--topo` |
| `relay = ["a:11010", "b:11010"]` | `--relay a:11010 --relay b:11010` |

收益:一份"生产环境该怎么跑"的知识可以随配置一起版本化,而不是散落在 systemd unit、Docker Compose 与运维手册三个地方。

```toml
# 生产环境的启动意图,随配置一起进版本控制
[cli]
log   = "info"
topo  = true
```

---

## 6. 配置项参考

以下所有配置项,**类型**为 TOML 类型,**级别**为热重载响应级别(`hot` / `reconnect` / `restart`,见第 7 章)。

### 6.0 顶层字段

与 `include` 并列的顶层字段。

| 配置项 | 类型 | 默认值 | 可选值 | 响应级别 | 说明 |
|---|---|---|---|---|---|
| `include` | string[] | `[]` | 文件路径 / 通配符 | `—`(合并期) | 引用其它配置文件 |
| `reset` | string[] | `[]` | 字段路径列表 | `—`(合并期) | 清空指定字段从低优先级来源继承的值 |

> `include` 和 `reset` 是**合并期元字段**,在配置加载阶段求值,本身没有运行时响应级别。被 `reset` 清空后重新定义的字段,响应级别按该字段自身的定义处理。

```toml
include = ["conf.d/*.toml", "secrets/*.toml"]
reset   = ["listen", "peers"]
```

`reset` 语义:

| 规则 | 说明 |
|---|---|
| 生效时机 | 在**当前文件参与的合并**开始前,先丢弃这些路径从低优先级来源继承的值 |
| 作用范围 | 只清空**低优先级来源**;更高优先级来源(ENV / CLI)不受影响 |
| 字段路径 | 点号分隔,如 `["routing.weight", "security.acl.rules"]` |
| 表与数组 | 对 map 清空整表,对数组清空整列表 |
| 不存在的路径 | 静默忽略,不报错 |
| 多文件声明 | 多个文件都写 `reset` 时取**并集** |
| 裸写 `[]` | **不清空**,仅追加零个元素 |
| **响应级别** | 不适用。合并期元字段,随加载流程重新求值;被清空后重新定义的字段按该字段自身级别处理(见 [第 7 章](#7-热重载)) |

典型用法——在覆盖文件中丢弃基础配置的监听与对端定义,完全重新声明:

```toml
# /etc/mesh/conf.d/99-override.toml
reset = ["listen", "peers"]

[[listen]]
addr = "udp://0.0.0.0:11010"

[peers]
relay = ["9.9.9.9:11010"]
```

改动后先跑 `mesh config diff` 确认影响范围——它显示的是**被 reset 清空后实际发生变化的字段**,而非 `reset` 本身。

### 6.1 network

网络身份与虚拟网段。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `network.name` | string | 无(**必填**) | 任意非空字符串 | `restart` | 网络名,同一网络必须一致 |
| `network.secret` | string | 无(**必填**) | 任意非空字符串 | `restart` | 网络密钥,准入凭证 + E2EE 派生材料 |
| `network.hostname` | string | 系统主机名 | 任意非空字符串 | `hot` | 节点显示名,仅用于展示 |
| `network.subnet` | string | `10.126.126.0/24` | 合法 CIDR | `restart` | 虚拟网段 |
| `network.mtu` | int | `1380` | 576 ~ 9000 | `reconnect` | 虚拟 MTU |
| `network.ipv6` | bool | `true` | `true` / `false` | `reconnect` | 是否启用 IPv6 候选地址 |
| `network.ipv6_only` | bool | `false` | `true` / `false` | `reconnect` | 仅使用 IPv6 |
| `network.node_id` | string | 自动派生 | 32 位十六进制 | `restart` | 手动指定 NodeID(**通常不要设置**) |

```toml
[network]
name       = "prod"
secret     = "change-me-to-a-strong-secret"
hostname   = "node-a"
subnet     = "10.126.126.0/24"
mtu        = 1380
ipv6       = true
ipv6_only  = false
```

要点:

- `network.mtu` 默认 `1380`,为 ZCPacket 头(48 字节)与传输层封装预留空间,避免分片。隧道类传输(ICMP/DNS/SSH)需要更小的 MTU。
- 网段 `10.126.126.0/24` 中,`10.126.126.1` 保留给控制面/网关角色。
- 虚拟 IP 由 NodeID 确定性派生,不需要也不支持手工分配。
- `network.node_id` 只应在迁移已有节点身份时使用;随意指定会导致身份冲突。

### 6.2 listen

本地监听地址与端口。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `listen.address` | string | `0.0.0.0` | 任意 IP 或 `::` | `reconnect` | 监听地址 |
| `listen.port` | int | `11010` | 1 ~ 65535 | `reconnect` | 主数据面端口(UDP/TCP 共用) |
| `listen.interface` | string | 空(全部) | 网卡名 | `reconnect` | 绑定到指定网卡 |
| `listen.port_range` | string | 空 | 如 `"11010-11020"` | `reconnect` | 端口预测的候选范围 |
| `listen.dual_stack` | bool | `true` | `true` / `false` | `reconnect` | 同时监听 IPv4 与 IPv6 |

```toml
[listen]
address    = "0.0.0.0"
port       = 11010
interface  = "eth0"
port_range = "11010-11020"
dual_stack = true
```

要点:

- 端口 `11010` 同时用于 UDP 与 TCP 监听。防火墙需要**同时放行两者**——只放 TCP 会导致打洞失败。
- `listen.interface` 在多网卡机器(如同时有内网与公网网卡)上有用,可避免把候选地址暴露在错误的网卡上。
- `listen.port_range` 为端口预测与生日悖论提供候选空间,范围越宽成功率越高,但需要在防火墙同步放行。

### 6.3 peers

静态对端与中继。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `peers.static` | string[] | `[]` | `host:port` 列表 | `hot` | 静态对端,绕过发现机制 |
| `peers.relay` | string[] | `[]` | `host:port` 列表 | `reconnect` | 中继服务器列表 |
| `peers.auto_discover` | bool | `true` | `true` / `false` | `hot` | 是否通过 LSA 自动发现对端 |
| `peers.max_peers` | int | `0`(不限) | ≥0 | `hot` | 最大对端数上限 |

```toml
[peers]
static         = ["10.0.0.5:11010", "203.0.113.9:11010"]
relay          = ["1.2.3.4:11010", "5.6.7.8:11010"]
auto_discover  = true
max_peers      = 0
```

要点:

- `peers.static` 用于没有发现机制也能连通的场景(如内网固定地址、控制面不可用时的应急直连)。
- `peers.relay` 可配多个,节点按延迟就近选择;至少配 2 个以容忍单点故障。
- 数组字段遵循**追加**合并:基础配置里的中继不会被后续文件覆盖,只会被补充。

### 6.4 ingress

接入模式与无 root 降级行为。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `ingress.mode` | string | `auto` | `tun` / `no-tun` / `auto` | `reconnect` | 接入模式 |
| `ingress.interface` | string | `mesh0` | 合法网卡名 | `reconnect` | TUN 接口名 |
| `ingress.socks5` | string | `127.0.0.1:1080` | `host:port` | `hot` | no-tun 模式的 SOCKS5 监听地址 |
| `ingress.socks5_enabled` | bool | `true` | `true` / `false` | `hot` | 是否启用 SOCKS5 |
| `ingress.forward` | string[] | `[]` | `tcp:本地=目标` 列表 | `hot` | 端口转发规则 |
| `ingress.subnet_proxy` | string[] | `[]` | CIDR 列表 | `hot` | 宣告进虚拟网络的物理子网 |
| `ingress.exit_node` | string | 空 | NodeID | `hot` | 指定出口节点 |
| `ingress.fallback` | bool | `true` | `true` / `false` | `reconnect` | 是否允许 auto 自动降级 |

```toml
[ingress]
mode            = "auto"
interface       = "mesh0"
socks5          = "127.0.0.1:1080"
socks5_enabled  = true
forward         = [
  "tcp:127.0.0.1:15432=10.126.126.42:5432",
  "tcp:127.0.0.1:16379=10.126.126.42:6379",
]
subnet_proxy    = ["192.168.1.0/24"]
exit_node       = ""
fallback        = true
```

要点:

- `auto` 优先尝试 TUN,失败后降级到 `no-tun` 并打印明确提示,**不静默降级**。
- `ingress.fallback = false` 时,auto 模式失败即报错退出。用于"必须确认跑在完整模式"的部署。
- `ingress.subnet_proxy` 需要本地路由表操作能力,因此**无 root 时不可用**。
- 端口转发规则的格式:`tcp:本地地址:本地端口=目标虚拟IP:目标端口`。

### 6.5 transports

传输协议选择与调优。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `transports.protocols` | string[] | `[]`(自动) | 见 [TRANSPORT.md](TRANSPORT.md) | `reconnect` | 协议候选列表,按序尝试 |
| `transports.auto_score` | bool | `true` | `true` / `false` | `hot` | 未指定时是否启用自动评分 |
| `transports.prefer_local` | bool | `true` | `true` / `false` | `hot` | 同机对端优先本地传输 |
| `transports.quic.max_streams` | int | `256` | ≥1 | `reconnect` | QUIC 最大并发流 |
| `transports.kcp.mode` | string | `fast` | `fast` / `normal` | `reconnect` | KCP 模式 |
| `transports.kcp.mtu` | int | `1380` | 576 ~ 1500 | `reconnect` | KCP MTU |
| `transports.kcp.window` | int | `512` | ≥1 | `reconnect` | KCP 窗口大小 |

```toml
[transports]
protocols    = ["udp", "quic", "tcp"]
auto_score   = true
prefer_local = true

[transports.quic]
max_streams = 256

[transports.kcp]
mode    = "fast"
mtu     = 1380
window  = 512
```

要点:

- `transports.protocols` 留空表示自动评分选择;显式指定时按序尝试,首个成功即用。
- 可用的协议名:`udp`、`tcp`、`quic`、`tls`、`ws`、`wss`、`http2`、`http3`、`grpc`、`kcp`、`dtls`、`wireguard`、`ssh`、`icmp`、`dns`、`unix`、`vsock`、`shm`。
- 隧道类协议(`icmp` / `dns` / `ssh`)吞吐与延迟明显劣于原生 UDP,自动评分会给它们最低优先级。
- 各协议的详细参数、适用场景与选型建议见 [TRANSPORT.md](TRANSPORT.md)。

### 6.6 nat

NAT 穿透策略。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `nat.stun` | string[] | 内置公共列表 | `host:port` 列表 | `hot` | STUN 服务器 |
| `nat.ice` | bool | `true` | `true` / `false` | `hot` | 启用 ICE |
| `nat.upnp` | bool | `true` | `true` / `false` | `hot` | 启用 UPnP 端口映射 |
| `nat.nat_pmp` | bool | `true` | `true` / `false` | `hot` | 启用 NAT-PMP |
| `nat.pcp` | bool | `true` | `true` / `false` | `hot` | 启用 PCP |
| `nat.punch_timeout` | duration | `5s` | 1s ~ 60s | `hot` | 打洞总超时 |
| `nat.repunch_interval` | duration | `60s` | 10s ~ 600s | `hot` | 中继状态下重打洞周期 |
| `nat.port_predict` | bool | `true` | `true` / `false` | `hot` | 启用端口预测 |
| `nat.birthday_paradox` | bool | `true` | `true` / `false` | `hot` | 启用生日悖论打洞 |
| `nat.ipv6_prefer` | bool | `true` | `true` / `false` | `hot` | IPv6 优先 |

```toml
[nat]
stun              = ["stun.limesh.dev:3478", "stun.cloudflare.com:3478"]
ice               = true
upnp              = true
nat_pmp           = true
pcp               = true
punch_timeout     = "5s"
repunch_interval  = "60s"
port_predict      = true
birthday_paradox  = true
ipv6_prefer       = true
```

要点:

- 所有打洞策略**并发发起**,首个成功即胜出。因此这些开关的取舍是"要不要多试一条路",而不是"要不要串行等它"。
- `nat.punch_timeout` 太短会在慢速链路上误判失败,太长会拖慢降级中继的时间。默认 5s 是折中。
- 中继状态下仍会按 `repunch_interval` 重试打洞,直连成功后自动切回,无需人工干预。
- 对称 NAT 环境下这些策略可能全部失败,此时中继路径是设计内的正常结果,不是故障。

### 6.7 routing

路由与路径选择。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `routing.strategy` | string | `latency` | `shortest` / `latency` / `weighted` | `hot` | 选路策略 |
| `routing.ewma_alpha` | float | `0.2` | 0.01 ~ 1.0 | `hot` | 延迟平滑系数 |
| `routing.multipath` | bool | `false` | `true` / `false` | `hot` | 启用多路径 |
| `routing.multipath_mode` | string | `flow` | `flow` / `packet` | `hot` | 多路径分流粒度 |
| `routing.lsa_interval` | duration | `10s` | 1s ~ 300s | `hot` | LSA 泛洪周期 |
| `routing.lsa_max_age` | duration | `300s` | 60s ~ 3600s | `hot` | LSA 失效时间 |
| `routing.keepalive_interval` | duration | `20s` | 1s ~ 300s | `hot` | 保活周期 |
| `routing.peer_degraded_after` | duration | `60s` | 10s ~ 600s | `hot` | 降级判定 |
| `routing.peer_dead_after` | duration | `120s` | 20s ~ 1200s | `hot` | 死亡判定 |
| `routing.weight` | table | `{}` | `NodeID = 权重` | `hot` | `weighted` 策略的权重表 |

```toml
[routing]
strategy             = "latency"
ewma_alpha           = 0.2
multipath            = true
multipath_mode       = "flow"
lsa_interval         = "10s"
lsa_max_age          = "300s"
keepalive_interval   = "20s"
peer_degraded_after  = "60s"
peer_dead_after      = "120s"

[routing.weight]
"7f3e9a0c41b25d88" = 10
"5c1d8f2ab9034e77" = 50
```

要点:

- `routing.ewma_alpha` 越小越平滑。路径抖动会导致 TCP 流在两条路径间迁移进而乱序,平滑是必需而非优化。
- `multipath_mode = "packet"` 会让同一 TCP 流乱序,**仅在承载纯 UDP 流量时**才建议开启;默认 `flow` 按五元组哈希分流,保留单流顺序性。
- `peer_degraded_after` 与 `peer_dead_after` 必须与 `keepalive_interval` 协调:`degraded = 3 × keepalive`,`dead = 6 × keepalive`。
- `weighted` 策略下未在 `weight` 表中出现的节点使用默认权重。

### 6.8 security

加密、访问控制与密钥轮换。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `security.e2ee` | bool | `true` | `true` / `false` | `restart` | 启用端到端加密 |
| `security.key_rotation.interval` | duration | `168h`(7 天) | 1h ~ 8760h | `hot` | 密钥轮换周期 |
| `security.key_rotation.overlap` | duration | `24h` | 0 ~ 720h | `hot` | 新旧密钥重叠窗口 |
| `security.acl.default` | string | `allow` | `allow` / `deny` | `hot` | ACL 默认动作 |
| `security.acl.rules` | table[] | `[]` | 见下 | `hot` | ACL 规则列表 |
| `security.audit.enabled` | bool | `false` | `true` / `false` | `hot` | 启用审计日志 |
| `security.audit.path` | string | `/var/lib/mesh/audit.log` | 文件路径 | `hot` | 审计日志路径 |

```toml
[security]
e2ee = true

[security.key_rotation]
interval = "168h"
overlap  = "24h"

[security.acl]
default = "deny"

[[security.acl.rules]]
action  = "allow"
from    = "10.126.126.0/24"
to      = "10.126.126.42"
ports   = [5432, 6379]
proto   = "tcp"

[[security.acl.rules]]
action  = "allow"
from    = "10.126.126.19"
to      = "192.168.1.0/24"
ports   = [22, 80, 443]
proto   = "tcp"

[security.audit]
enabled = true
path    = "/var/lib/mesh/audit.log"
```

要点:

- `security.e2ee` 关闭后中继可解密流量,**仅用于调试**,生产环境不要关闭。
- `security.acl` 规则按**顺序**匹配,首条命中即生效。数组追加语义意味着基础规则在前、追加规则在后,追加规则无法覆盖基础规则——需要覆盖时请调整基础文件。
- `default = "deny"` 是更安全的起点:先全部拒绝,再逐条放行。
- 密钥轮换的 `overlap` 窗口内新旧密钥并存,保证轮换过程中不中断既有连接。

### 6.9 observability

指标、拓扑与日志导出。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `observability.metrics.enabled` | bool | `false` | `true` / `false` | `hot` | 启用 Prometheus 端点 |
| `observability.metrics.listen` | string | `127.0.0.1:9100` | `host:port` | `hot` | 指标监听地址 |
| `observability.metrics.path` | string | `/metrics` | URL 路径 | `hot` | 指标路径 |
| `observability.topo_api.enabled` | bool | `false` | `true` / `false` | `hot` | 启用拓扑 API |
| `observability.topo_api.listen` | string | `127.0.0.1:9101` | `host:port` | `hot` | 拓扑 API 监听地址 |
| `observability.topo_api.token` | string | 空 | 任意字符串 | `hot` | API 访问令牌(Bearer) |
| `observability.admin_api.enabled` | bool | `false` | `true` / `false` | `hot` | 启用 Admin API |
| `observability.admin_api.listen` | string | `127.0.0.1:9102` | `host:port` | `hot` | Admin API 监听地址 |

```toml
[observability.metrics]
enabled = true
listen  = "127.0.0.1:9100"
path    = "/metrics"

[observability.topo_api]
enabled = true
listen  = "127.0.0.1:9101"
token   = "topo-read-token"

[observability.admin_api]
enabled = true
listen  = "127.0.0.1:9102"
```

要点:

- 默认监听 `127.0.0.1` 而非 `0.0.0.0`,避免指标与拓扑信息意外暴露到公网。需要外部抓取时再显式改地址,并配合防火墙与令牌。
- Admin API 提供 `POST /api/v1/config/reload` 等写操作,**必须**绑定在受控地址上。
- 验证指标可用:

```bash
curl -s localhost:9100/metrics | grep '^mesh_'
```

```
mesh_peer_total 12
mesh_peer_direct 9
mesh_peer_relayed 3
mesh_packet_sent_total 1842391
```

### 6.10 log

日志输出。

| 配置项 | 类型 | 默认值 | 可选值 | 级别 | 说明 |
|---|---|---|---|---|---|
| `log.level` | string | `info` | `trace` / `debug` / `info` / `warn` / `error` | `hot` | 日志级别 |
| `log.format` | string | `text` | `text` / `json` | `hot` | 输出格式 |
| `log.output` | string | `stderr` | `stderr` / `stdout` / 文件路径 | `hot` | 输出目标 |
| `log.file_rotation.max_size` | int | `100` | ≥1(MB) | `hot` | 单文件最大体积 |
| `log.file_rotation.max_backups` | int | `5` | ≥0 | `hot` | 保留的历史文件数 |
| `log.file_rotation.compress` | bool | `true` | `true` / `false` | `hot` | 是否压缩历史文件 |

```toml
[log]
level  = "info"
format = "json"
output = "/var/log/mesh/mesh.log"

[log.file_rotation]
max_size    = 100
max_backups = 5
compress    = true
```

要点:

- `log.level = "trace"` 会产生大量输出,**排错时临时使用**,不要长期开启。
- `format = "json"` 便于接入日志采集系统;`text` 便于人工阅读。
- `log.level` 是最常用的热重载目标:改完保存即生效,连接不中断。

---

## 7. 热重载

三个触发入口:

| 入口 | 用法 | 适用 |
|---|---|---|
| 文件监听 | 编辑配置文件保存,自动触发 | 交互式运维 |
| 信号 | `kill -HUP $(pidof mesh)` 或 `systemctl reload mesh` | 脚本、配置管理系统 |
| Admin API | `POST /api/v1/config/reload` | 远程编排、自动化平台 |

```bash
# 信号
sudo systemctl reload mesh

# 命令行(等价于向本机实例发信号并等待结果)
mesh config reload

# Admin API
curl -X POST http://127.0.0.1:9102/api/v1/config/reload
```

### 7.1 执行流程

```
触发 → 重新解析全部来源
     → 展开 include(得到实际加载的文件列表与顺序)
     → 执行 reset(丢弃被指定路径从低优先级来源继承的值)
     → 合并四层来源 → 生成新 Config 快照
     → 与旧快照逐模块 diff → 按 hot / reconnect / restart 分级
     → 逐模块 Apply
         ├─ 全部成功 → 原子替换快照,写审计日志
         └─ 任一失败 → 回滚已 Apply 的模块 → 保留旧快照 → 报错
```

热重载会**完整重跑加载流程**,而不是增量应用文件差异。`include` 与 `reset` 都在这一步重新求值,因此:

- 新增或删除被 `include` 匹配到的文件,热重载能感知。
- 修改 `reset` 列表本身,也能在热重载时生效。
- `reset` 与 `include` 都不产生"变更事件",它们只影响**合并的输入**。

四条不可违背的约束:

1. **原子替换** —— 运行期读到的永远是一份完整快照,不存在"改了一半"的中间态。
2. **失败回滚** —— Apply 失败的模块必须恢复到旧值(`Apply` 与 `Revert` 成对实现)。
3. **restart 级变更不自动重启** —— 标记为 `PendingRestart`,由 `mesh config diff` / `mesh status` 提示。
4. **失败不退出进程** —— 热重载失败只记录错误并保持旧配置继续服务。

### 7.2 reset 在热重载中的行为

`reset` 是合并期元字段,**自身没有响应级别**,也不会出现在 `mesh config diff` 的变更清单里。它只决定"哪些继承值被丢弃",真正触发 Apply 的是被清空后**重新定义出来的实际字段**。

```
reset 本身          → 无变更事件,不触发任何 Apply
被 reset 清空的字段 → 按该字段自身的响应级别处理
```

举例说明:

| 配置变更 | 实际变化 | 响应级别 | 结果 |
|---|---|---|---|
| `reset = ["peers"]` 后重新定义 `[peers] relay = [...]` | `peers.relay` 从旧值变为新值 | `reconnect` | 重建中继连接 |
| `reset = ["listen"]` 后重新定义 `[[listen]] addr = ...` | `listen.*` 变化 | `reconnect` | 关闭旧 socket,按新参数监听 |
| `reset = ["network.name"]` 后重新定义 `name = "prod2"` | `network.name` 变化 | `restart` | 标记 `PendingRestart`,**拒绝热重载** |
| `reset = ["log"]`,但未重新定义 `[log]` | `log.*` 回到默认值 | `hot` | 日志级别等原地生效 |
| `reset = ["peers"]`,但未重新定义 `[peers]` | `peers.*` 被清空 | 按各字段级别 | 对端列表清空,按字段级别应用或待重启 |

注意第三种情况:**`reset` 可以间接触发 `restart`**。`reset` 本身不是 `restart` 级,但如果它清空并重新定义的是一个 `restart` 级字段(如 `network.name`、`network.secret`、`network.subnet`),结果仍然是拒绝热重载、标记待重启。

判断某次 `reset` 改动的影响:

```bash
mesh config diff
```

```
peers.relay      ["1.2.3.4:11010"] → ["9.9.9.9:11010"]   reconnect
log.level        "debug" → "info"                        hot
network.secret   changed                                 requires restart
```

输出的永远是**字段级**变更,不会出现 `reset` 这一行。这是设计使然:`reset` 描述的是"合并怎么做",而非"配置是什么"。

### 7.3 响应级别

| 级别 | 语义 | 连接影响 |
|---|---|---|
| `hot` | 原地生效 | 无,连接不中断 |
| `reconnect` | 重建传输 | 瞬时中断,虚拟 IP 与路由保留 |
| `restart` | 需要重启进程 | 不自动应用,标记 `PendingRestart` |

### 7.4 逐项级别对照表

**`hot` —— 原地生效**

| 配置项 |
|---|
| `network.hostname` |
| `peers.static` |
| `peers.auto_discover` |
| `peers.max_peers` |
| `ingress.socks5` |
| `ingress.socks5_enabled` |
| `ingress.forward` |
| `ingress.subnet_proxy` |
| `ingress.exit_node` |
| `transports.auto_score` |
| `transports.prefer_local` |
| `nat.*`(全部) |
| `routing.*`(全部) |
| `security.key_rotation.*` |
| `security.acl.*` |
| `security.audit.*` |
| `observability.*`(全部) |
| `log.*`(全部) |

**`reconnect` —— 重建传输**

| 配置项 | 行为 |
|---|---|
| `network.mtu` | 重建 TUN 接口,MTU 变更;连接重连 |
| `network.ipv6` / `network.ipv6_only` | 重新枚举候选地址 |
| `listen.address` / `listen.port` / `listen.interface` / `listen.port_range` / `listen.dual_stack` | 关闭旧 socket,按新参数重新监听 |
| `peers.relay` | 断开旧中继,连接新中继列表 |
| `ingress.mode` / `ingress.interface` / `ingress.fallback` | 切换接入层实现 |
| `transports.protocols` | 协商新的协议集合 |
| `transports.quic.*` / `transports.kcp.*` | 以新参数重建对应传输 |

**`restart` —— 需要重启进程**

| 配置项 | 为什么必须重启 |
|---|---|
| `network.name` | 网络标识参与握手与 LSA,运行期变更会造成身份不一致 |
| `network.secret` | 密钥派生材料,变更等价于换网络 |
| `network.subnet` | 虚拟网段变更影响所有已派生的虚拟 IP |
| `network.node_id` | 节点身份本身 |
| `security.e2ee` | 加解密路径开关,涉及所有会话状态 |

`restart` 级变更不会静默生效:

```bash
mesh config diff
```

```
network.secret   changed    requires restart
log.level        changed    hot (applied)
```

### 7.5 热重载演示

```bash
# 1. 查看当前值
mesh config show | grep -A2 '\[log\]'
```

```toml
[log]
level = "info"
```

```bash
# 2. 修改文件(或执行 mesh config reload)
sudo sed -i 's/level = "info"/level = "debug"/' /etc/mesh/config.toml

# 3. 观察日志:连接数为 0
sudo journalctl -u mesh -f
```

```
INFO  config  reload triggered   source=fsnotify file=/etc/mesh/config.toml
INFO  config  diff               log.level: info → debug        [hot]
INFO  config  applied            1 module, 0 failed, 0 pending-restart
```

失败的例子(把 `secrets/` 文件权限改错后重载):

```bash
sudo chmod 644 /etc/mesh/secrets/prod.toml
mesh config reload
```

```
ERROR config  reload failed      secrets/prod.toml: permission 0644, require 0600
ERROR config  rollback           reverted 0 module(s)
INFO  config  running config unchanged (previous snapshot still active)
```

进程继续以旧配置服务,退出码非零供脚本判断。

---

## 8. 安全

### 8.1 权限检查

启动时与热重载时都执行:

| 检查项 | 要求 | 违规处理 |
|---|---|---|
| `secrets/*.toml` 文件权限 | 必须为 `0600` | **拒绝启动 / 拒绝重载** |
| 密钥文件所在目录 | 建议 `0700` | 警告 |
| 配置文件是否为符号链接 | 允许,但记录真实路径 | 仅记录 |
| 配置文件属主 | 应为运行用户 | 警告 |

```bash
chmod 700 /etc/mesh/secrets
chmod 600 /etc/mesh/secrets/prod.toml

ls -la /etc/mesh/secrets/
```

```
drwx------ 2 root root 4096 10月  6 20:31 .
-rw------- 1 root root   58 10月  6 20:31 prod.toml
```

为什么 `secrets/` 是强制而其它只是警告:配置文件里会出现网络密钥,而密钥一旦被同机其他用户读取,整个虚拟网络的身份隔离就失效了——此时加密形同虚设。相对地,配置文件属主不对只是配置管理问题,不影响安全边界。

**命名约定即检查范围**:权限强制只作用于 `secrets/` 目录下的 `.toml` 文件。请把密钥放在这个目录里,不要散落在普通配置文件、环境变量文件或 systemd unit 中。

### 8.2 密钥管理建议

```bash
# 生成强密钥
head -c 32 /dev/urandom | base64
```

```toml
# /etc/mesh/secrets/prod.toml
[network]
secret = "kR8vQ2mXpL5nW7zT4yB6cD1fG3hJ9aM0sE2uI5oP7qR="
```

| 建议 | 理由 |
|---|---|
| 用 `base64` 或十六进制随机串,长度 ≥ 32 字节 | 密钥强度直接决定网络准入安全性 |
| 不要复用其它服务的密钥 | 一旦泄漏,爆炸半径最小化 |
| 不写进版本控制 | `secrets/` 应加入 `.gitignore` |
| 定期轮换(`security.key_rotation.interval`) | 限制泄漏后的有效时间窗口 |
| 轮换时保留 `overlap` 窗口 | 避免轮换瞬间全体断连 |

轮换演示:

```bash
# 1. 生成新密钥并写入 secrets 文件(保持 0600)
NEW=$(head -c 32 /dev/urandom | base64)
sudo sh -c "printf '[network]\nsecret = \"%s\"\n' '$NEW' > /etc/mesh/secrets/prod.toml"
sudo chmod 600 /etc/mesh/secrets/prod.toml

# 2. 热重载触发轮换(节点身份不变,虚拟 IP 不变)
mesh config reload
```

```
WARN  config  network.secret changed  requires restart
```

密钥属于 `restart` 级,因此需要**逐节点滚动重启**而非热重载。正确的轮换顺序是:

```
1. 新旧密钥并存(overlap 窗口)
2. 逐节点替换密钥并重启,每台之间确认 mesh peer list 正常
3. 全部完成后移除旧密钥
```

`security.key_rotation.interval` 提供的是自动轮换:节点自行生成新密钥并通过已加密的信道分发给对端,无需人工逐台操作。

---

## 9. 配置管理命令

| 命令 | 作用 |
|---|---|
| `mesh config show` | 显示合并后的最终配置 |
| `mesh config raw` | 显示原始文件内容,不做合并 |
| `mesh config sources` | 显示每一项的来源 |
| `mesh config validate` | 校验配置合法性,不启动 |
| `mesh config diff` | 对比运行中配置与文件差异 |
| `mesh config export` | 导出当前生效配置 |
| `mesh config expand` | 展开 include 与通配符,显示实际加载的文件 |

### 9.1 `mesh config show`

显示**合并后**的最终配置,即进程真正在用的值。

```bash
mesh config show
```

```toml
[network]
name     = "prod"
hostname = "node-a"
subnet   = "10.126.126.0/24"
mtu      = 1380

[ingress]
mode = "auto"

[log]
level = "info"
```

常用参数:

```bash
mesh config show --json          # JSON 输出,便于 jq 处理
mesh config show --sources       # 附加来源信息
mesh config show --section network   # 只看某一节
```

```bash
mesh config show --json | jq '.network.mtu'
```

```
1380
```

注意:`show` **不显示** `network.secret` 的明文,只显示是否已设置。

### 9.2 `mesh config raw`

显示原始文件内容,**不做合并**,用于确认某个文件到底写了什么。

```bash
mesh config raw -c /etc/mesh/config.toml
```

```toml
include = ["conf.d/*.toml", "secrets/*.toml"]

[cli]
log = "info"

[network]
name = "prod"
```

与 `show` 的区别:`raw` 看到的是文件字面内容,`show` 看到的是合并结果。排查"这个字段到底有没有被写进去"用 `raw`,排查"最终值是多少"用 `show`。

### 9.3 `mesh config sources`

显示每一项的**来源**。这是排错的第一工具。

```bash
mesh config sources
```

```
network.name      ← --network (命令行)
network.secret    ← /etc/mesh/secrets/prod.toml:2
network.subnet    ← /etc/mesh/config.toml:7
network.mtu       ← MESH_NETWORK_MTU
log.level         ← [cli] 块 (/etc/mesh/config.toml:5)
routing.strategy  ← 默认值
```

```bash
mesh config sources --format json
```

```json
[
  { "key": "network.name", "value": "prod", "source": "cli", "origin": "--network" },
  { "key": "network.mtu",  "value": 1380,  "source": "env", "origin": "MESH_NETWORK_MTU" },
  { "key": "log.level",    "value": "info","source": "default", "origin": null }
]
```

来源类型:`default` / `file` / `cli-block` / `env` / `cli`。

`mesh config show --sources` 是同一功能的另一种调用形式,输出格式略紧凑。

### 9.4 `mesh config validate`

校验配置的**语法与语义**合法性,但不启动节点。用于 CI 与部署前检查。

```bash
mesh config validate -c /etc/mesh/config.toml
```

```
OK  /etc/mesh/config.toml
    include  resolved 5 file(s)
    sections network, ingress, log, routing
    no warnings
```

失败示例:

```bash
mesh config validate -c bad.toml
```

```
ERROR bad.toml:7  network.subnet  "10.126.126.0/33" is not a valid CIDR
ERROR bad.toml:12 network.secret  required field is missing
ERROR bad.toml:19 nat.punch_timeout  "0s" out of range [1s, 60s]
FAILED  3 error(s), 0 warning(s)
```

退出码:`0` 通过,`1` 有错误,`2` 文件无法读取。适合直接接入 CI:

```bash
mesh config validate -c /etc/mesh/config.toml || exit 1
```

### 9.5 `mesh config diff`

对比**运行中配置**与**文件配置**的差异。

```bash
mesh config diff -c /etc/mesh/config.toml
```

```
log.level         info → debug          hot
routing.strategy  latency → shortest    hot
network.mtu       1380 → 1400           reconnect
network.secret    changed               requires restart
```

不带 `-c` 时使用已加载的配置文件列表。这条命令回答两个问题:

1. 我改了文件,哪些项会变?
2. 有没有需要重启才能生效的项?

```bash
# 检查是否存在待重启项(脚本友好的做法)
mesh config diff --quiet --pending-restart-only && echo "无需重启" || echo "需要重启"
```

### 9.6 `mesh config export`

导出**当前生效的完整配置**,包括来自 CLI、ENV、默认值的部分,输出为一份自包含的 TOML。

```bash
mesh config export > /etc/mesh/config.snapshot.toml
```

```toml
# 由 mesh config export 生成于 2026-10-06T20:31:00+08:00
# 该文件包含所有生效值,可直接用于复现当前配置

[network]
name     = "prod"
secret   = "*** masked ***"
hostname = "node-a"
subnet   = "10.126.126.0/24"
mtu      = 1380
# ...
```

用途:

| 场景 | 做法 |
|---|---|
| 复现线上配置 | 导出后在测试环境直接使用 |
| 迁移节点 | 导出 → 替换密钥 → 部署到新机器 |
| 配置审计 | 纳入版本控制,对比历史差异 |
| 事故排查 | 保存故障时刻的完整配置快照 |

`secret` 默认以 `*** masked ***` 导出。需要导出明文时显式声明:

```bash
mesh config export --include-secrets > /etc/mesh/config.snapshot.toml
chmod 600 /etc/mesh/config.snapshot.toml
```

导出含密钥的文件**必须**设成 `0600`,并避免留在共享目录。

### 9.7 `mesh config expand`

展开 `include` 与通配符,**显示实际被加载的文件及顺序**。

```bash
mesh config expand -c /etc/mesh/config.toml
```

```
/etc/mesh/config.toml
├── /etc/mesh/conf.d/10-network.toml
├── /etc/mesh/conf.d/20-ingress.toml
├── /etc/mesh/conf.d/30-transports.toml
├── /etc/mesh/conf.d/40-observability.toml
└── /etc/mesh/secrets/prod.toml
```

```
load order (later overrides earlier):
  1. /etc/mesh/conf.d/10-network.toml
  2. /etc/mesh/conf.d/20-ingress.toml
  3. /etc/mesh/conf.d/30-transports.toml
  4. /etc/mesh/conf.d/40-observability.toml
  5. /etc/mesh/secrets/prod.toml
```

这条命令回答三个问题:

1. 通配符到底匹配到了哪些文件?
2. 加载顺序是什么(谁覆盖谁)?
3. 有没有匹配到不该加载的文件?

JSON 形式便于脚本处理:

```bash
mesh config expand -c /etc/mesh/config.toml --format json | jq -r '.files[].path'
```

### 9.8 命令速查

```bash
mesh config show                      # 最终生效配置
mesh config show --sources            # 带来源
mesh config raw -c FILE               # 原始文件内容
mesh config sources                   # 仅来源列表
mesh config validate -c FILE          # 校验
mesh config diff -c FILE              # 与运行配置对比
mesh config export > snapshot.toml    # 导出完整配置
mesh config expand -c FILE            # 展开 include 与实际加载顺序
mesh config reload                    # 触发热重载
```

---

## 10. 完整示例

以下是一份覆盖大部分字段的完整配置,可直接作为起点。

```toml
# ============================================================
# /etc/mesh/config.toml —— Limesh 完整配置示例
# ============================================================

# 引用其它配置文件(相对当前文件所在目录)
include = ["conf.d/*.toml", "secrets/*.toml"]

# ------------------------------------------------------------
# [cli] —— 内嵌命令行参数,优先级高于下方文件字段
# ------------------------------------------------------------
[cli]
log  = "info"
topo = true

# ------------------------------------------------------------
# [network] —— 网络身份与虚拟网段
# ------------------------------------------------------------
[network]
name      = "prod"                              # 必填,同网络必须一致
secret    = "change-me-to-a-strong-secret"      # 必填,建议放 secrets/
hostname  = "node-a"                            # 展示用主机名
subnet    = "10.126.126.0/24"                   # 虚拟网段
mtu       = 1380                                # 虚拟 MTU
ipv6      = true                                # 启用 IPv6 候选地址
ipv6_only = false                               # 不强制仅 IPv6

# ------------------------------------------------------------
# [listen] —— 本地监听
# ------------------------------------------------------------
[listen]
address    = "0.0.0.0"
port       = 11010
interface  = ""                                 # 空 = 全部网卡
port_range = "11010-11020"                      # 端口预测候选范围
dual_stack = true

# ------------------------------------------------------------
# [peers] —— 静态对端与中继
# ------------------------------------------------------------
[peers]
static        = ["10.0.0.5:11010"]              # 静态对端,绕过发现
relay         = ["1.2.3.4:11010", "5.6.7.8:11010"]  # 中继,建议 ≥2 个
auto_discover = true
max_peers     = 0                               # 0 = 不限

# ------------------------------------------------------------
# [ingress] —— 接入模式
# ------------------------------------------------------------
[ingress]
mode           = "auto"                         # tun / no-tun / auto
interface      = "mesh0"
socks5         = "127.0.0.1:1080"
socks5_enabled = true
forward        = [
  "tcp:127.0.0.1:15432=10.126.126.42:5432",
]
subnet_proxy   = ["192.168.1.0/24"]             # 需 root
exit_node      = ""                             # 出口节点 NodeID
fallback       = true                           # 允许 auto 降级

# ------------------------------------------------------------
# [transports] —— 传输协议
# ------------------------------------------------------------
[transports]
protocols    = ["udp", "quic", "tcp"]           # 空 = 自动评分
auto_score   = true
prefer_local = true

[transports.quic]
max_streams = 256

[transports.kcp]
mode   = "fast"
mtu    = 1380
window = 512

# ------------------------------------------------------------
# [nat] —— NAT 穿透
# ------------------------------------------------------------
[nat]
stun             = ["stun.limesh.dev:3478", "stun.cloudflare.com:3478"]
ice              = true
upnp             = true
nat_pmp          = true
pcp              = true
punch_timeout    = "5s"
repunch_interval = "60s"
port_predict     = true
birthday_paradox = true
ipv6_prefer      = true

# ------------------------------------------------------------
# [routing] —— 路由与路径选择
# ------------------------------------------------------------
[routing]
strategy            = "latency"                 # shortest / latency / weighted
ewma_alpha          = 0.2
multipath           = false
multipath_mode      = "flow"                    # flow / packet
lsa_interval        = "10s"
lsa_max_age         = "300s"
keepalive_interval  = "20s"
peer_degraded_after = "60s"
peer_dead_after     = "120s"

[routing.weight]
# "7f3e9a0c41b25d88" = 10                       # weighted 策略使用

# ------------------------------------------------------------
# [security] —— 加密、ACL、密钥轮换
# ------------------------------------------------------------
[security]
e2ee = true

[security.key_rotation]
interval = "168h"
overlap  = "24h"

[security.acl]
default = "deny"

[[security.acl.rules]]
action = "allow"
from   = "10.126.126.0/24"
to     = "10.126.126.42"
ports  = [5432, 6379]
proto  = "tcp"

[[security.acl.rules]]
action = "allow"
from   = "10.126.126.19"
to     = "192.168.1.0/24"
ports  = [22, 80, 443]
proto  = "tcp"

[security.audit]
enabled = true
path    = "/var/lib/mesh/audit.log"

# ------------------------------------------------------------
# [observability] —— 指标与 API
# ------------------------------------------------------------
[observability.metrics]
enabled = true
listen  = "127.0.0.1:9100"
path    = "/metrics"

[observability.topo_api]
enabled = true
listen  = "127.0.0.1:9101"
token   = "topo-read-token"

[observability.admin_api]
enabled = false
listen  = "127.0.0.1:9102"

# ------------------------------------------------------------
# [log] —— 日志
# ------------------------------------------------------------
[log]
level  = "info"                                 # trace/debug/info/warn/error
format = "text"                                 # text / json
output = "stderr"

[log.file_rotation]
max_size    = 100
max_backups = 5
compress    = true
```

配套的密钥文件:

```toml
# /etc/mesh/secrets/prod.toml
# 权限必须为 0600,否则 Limesh 拒绝启动
[network]
secret = "kR8vQ2mXpL5nW7zT4yB6cD1fG3hJ9aM0sE2uI5oP7qR="
```

部署检查清单:

```bash
# 1. 权限
chmod 700 /etc/mesh/secrets
chmod 600 /etc/mesh/secrets/prod.toml

# 2. 校验配置
mesh config validate -c /etc/mesh/config.toml

# 3. 确认加载顺序
mesh config expand -c /etc/mesh/config.toml

# 4. 确认最终值
mesh config show --sources | head -20

# 5. 启动
sudo mesh node -c /etc/mesh/config.toml
```

---

## 11. 故障排查

### 11.1 配置改了没生效

**九成是优先级问题。** 第一件事永远是查来源:

```bash
mesh config sources | grep <你改的那一项>
```

```
log.level  ← --log (命令行)          # 命令行覆盖了文件
```

对照 [第 3 章](#3-优先级)的五层顺序检查:命令行 > 环境变量 > `[cli]` 块 > 文件字段 > 默认值。

常见变体:

| 现象 | 原因 |
|---|---|
| 改了文件,值不变 | 有 ENV 或 CLI 覆盖,或改了错误的文件 |
| 改了文件,重启后恢复 | 改动写在了 `config export` 的产物里,被重新生成的覆盖 |
| 值变了又变回去 | 有多个文件都设置了该项,加载顺序与预期不同 |

确认实际加载了哪些文件:

```bash
mesh config expand -c /etc/mesh/config.toml
```

### 11.2 通配符没匹配到文件

```bash
mesh config expand -c 'conf.d/*.toml'
```

```
/etc/mesh/config.toml
└── (no include resolved)
```

排查顺序:

```bash
# 1. 通配符是否被 shell 提前展开?必须加引号
mesh node -c 'conf.d/*.toml'      # ✅ 正确
mesh node -c conf.d/*.toml        # ❌ shell 会先展开

# 2. 相对路径的基准是什么?
#    -c 的参数相对于工作目录
#    include 中的路径相对于当前文件所在目录

# 3. 文件扩展名必须是 .toml
ls conf.d/
```

注意:`-c` 的通配符相对于**进程工作目录**;`include` 内的通配符相对于**当前文件所在目录**。两者基准不同,是最容易混淆的地方。

### 11.3 secrets 权限报错

```
ERROR config  secrets/prod.toml: permission 0644, require 0600
```

```bash
chmod 600 /etc/mesh/secrets/prod.toml
chmod 700 /etc/mesh/secrets
mesh config validate -c /etc/mesh/config.toml
```

如果使用 Docker 挂载,注意宿主机的权限会原样出现在容器内:

```bash
ls -la /etc/mesh/secrets/prod.toml
# -rw------- 1 root root 58 ...   ✅
```

Docker 中常见问题:宿主机文件属主是 UID 1000,容器内以 root 运行,权限判断可能按容器内视角进行。稳妥做法是挂载前统一权限。

### 11.4 restart 级变更没生效

```bash
mesh config diff
```

```
network.secret  changed  requires restart
```

`restart` 级变更不自动应用。处理方式:

```bash
sudo systemctl restart mesh
```

或在 systemd 环境下直接 `systemctl reload` 后观察提示,确认标记为 `PendingRestart` 的项:

```bash
mesh status | grep -i restart
```

```
PendingRestart  network.secret
```

### 11.5 热重载失败但进程还活着

这是**预期行为**。热重载失败不会导致进程退出,而是保留旧配置继续服务。

```bash
mesh config reload
echo "exit=$?"
```

```
ERROR config  reload failed   routing.ewma_alpha: 1.5 out of range [0.01, 1.0]
ERROR config  rollback        reverted 0 module(s)
INFO  config  running config unchanged
exit=1
```

检查点:

| 检查 | 命令 |
|---|---|
| 配置是否合法 | `mesh config validate -c FILE` |
| 哪些值要被改 | `mesh config diff -c FILE` |
| 是否有待重启项 | `mesh config diff`(看 requires restart) |
| 具体报错 | `journalctl -u mesh -n 50` |

### 11.6 校验通过但启动后行为不符预期

```bash
# 1. 确认最终生效值
mesh config show

# 2. 确认来源
mesh config sources

# 3. 确认接入模式是否降级
mesh status | grep -i ingress
```

```
Ingress      no-tun (gVisor netstack)     ← auto 模式下 TUN 不可用,已降级
```

降级是 `auto` 模式的正常行为。日志中会有明确提示:

```
INFO  ingress  tun  unavailable: operation not permitted
INFO  ingress  fallback   auto → no-tun (gVisor netstack)
WARN  ingress  limited mode: no transparent routing, use SOCKS5 or --forward
```

要禁止降级(失败即退出):

```toml
[ingress]
mode     = "auto"
fallback = false
```

### 11.7 两个节点配置"一样"却连不上

配置内容相同不代表**生效值**相同。逐项对比:

```bash
# 在两台机器上分别执行,对比输出
mesh config show --sources --json | jq '.network, .ingress, .transports, .nat'
```

重点核对:

| 项 | 检查点 |
|---|---|
| `network.name` | 必须完全一致 |
| `network.secret` | 必须完全一致(最常见原因) |
| `network.subnet` | 必须一致,否则虚拟 IP 不在同一网段 |
| `transports.protocols` | 双方至少有一个交集 |
| `nat.*` | 不一致会导致打洞策略不对称,成功率下降 |

用 `expand` 确认两边加载了同一组文件:

```bash
mesh config expand -c /etc/mesh/config.toml
```

---

**文档版本**:v1.0
**对应总纲**:[AGENTS.md](../AGENTS.md) 第 10 章
**相关文档**:[TRANSPORT.md](TRANSPORT.md) · [README.md](../README.md)
**许可证**:Apache 2.0
