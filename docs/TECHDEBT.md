# Limesh 技术债

> 本文件记录已知的、**有意保留到后续阶段解决**的技术债。每条都包含:
> 现状、影响、建议的解决方向。
>
> 与代码中 `TODO` 注释的区别:本文件记录的是**设计层面的取舍**,
> 需要专门的重构或阶段工作,而不是随手可改的小问题。

---

## 1. `handshake.Close` 的语义需要重构

**现状**

`core/handshake` 的 `Handshake` 对象在握手成功后会**持有**派生出的
`*session.Session`。其 `Close()` 方法会**连带关闭这个 session**:

```go
func (h *Handshake) Close() {
    if h.session != nil {
        h.session.Close()   // ← 连会话一起关
        h.session = nil
    }
    ...
}
```

这导致一个隐蔽的错误:数据面在握手成功后,先把会话 `sessions.Add` 进池,
再 `hs.Close()` 清理握手上下文——结果刚加入池的会话立刻被关掉,第一个数据包
就报 `session: closed`。现象是"握手成功但数据不通",与 Noise 密钥方向错误
(见交接文档 §7.1)的表现高度相似,排查成本很高。

**当前规避**

`core/handshake` 新增了 `DetachSession()`:把会话所有权从握手对象移出并返回,
之后 `Close()` 不再影响它。数据面 `adoptHandshake` 使用该方法的正确顺序是:

```go
s, _ := fh.hs.DetachSession()   // 先取走所有权
// ... 加入会话池、注册 Peer ...
fh.hs.Close()                   // 再关握手上下文,不再碰 session
```

**问题**

`DetachSession` 是**临时规避**,不是好的接口设计:

- 它要求每个调用方记住"必须先 detach 再 close"这一顺序约束,容易再次踩坑;
- 一个对象的 `Close` 关闭了"不属于它"的资源,违反最小惊讶原则;
- 会话的所有权在握手对象与数据面之间被隐式转移,生命周期不清晰。

**建议方向**

让 `Handshake` 的 `Close()` **只清理握手上下文**(Noise 状态、临时密钥、
时间戳集合),**不触碰**会话。会话一旦派生,所有权即归调用方,无需 `DetachSession`。

- 方案 A:握手不再持有会话,改为 `DeriveSession() (*session.Session, error)`
  在完成时一次性产出并返回,内部不留引用。
- 方案 B:保留内部引用,但 `Close()` 只置空引用、不调用 `session.Close()`。

落地后应删除 `DetachSession`,并移除调用方对顺序的依赖。**属 Phase 2 重构项。**

---

## 2. Peer 状态命名风格不统一

**现状**

`core/peer` 的状态名直接来自 AGENTS.md §8.9.1 的原始状态图:

| 状态 | 词形 |
|---|---|
| `Idle` | 形容词 |
| `Handshake` | 名词(原形) |
| `Probing` | 进行式 |
| `Direct` | 形容词 |
| `Relayed` | 过去式 |
| `Degraded` | 过去分词 |
| `Dead` | 形容词 |
| `Failed` | 过去分词 |

`Handshake`(原形)、`Probing`(进行式)、`Relayed`(过去式)三者词形不一致。

**当前处理**

按 §2"命名是项目内契约,不允许各模块自行发明别名",**保持现状**。
状态名已分散在状态图、常量、日志、指标(`mesh_peer_relayed` 等)与文档中,
随意改名会牵动多处且与既有文档不符。

已在代码与 AGENTS.md §8.9.1.1 中以注释形式明确说明这是**有意保留的历史命名**,
而非疏漏。

**建议方向**

若未来需要统一,建议一次性重命名为一致的词形(如全部用过去分词:
`Idled / Handshaked / Probed / Connected / Relayed / Degraded / Dead / Failed`),
并同步更新:AGENTS.md 状态图、常量、指标名、CLI 输出与所有测试。
这是一次性、机械但覆盖面广的改动,**不应急于在 Phase 1a/2 进行**。

---

## 3. `PeerSpec` 手工注入公钥(临时契约)

**现状**

Phase 1a 无控制面/发现通道,发起方无法自动获知响应方公钥。因此
`node.PeerSpec{Addr, PublicKey}` 由调用方显式注入。

**影响**

真实部署中不存在"调用方手工填公钥"的角色;这使 Phase 1a 只能用于测试与
受控双节点场景。

**建议方向**

Phase 2 的控制面完成后,由 controller 或配置文件下发对端公钥,
`PeerSpec` 退化为内部类型或移除。详见 AGENTS.md §8.9.3。

---

## 4. `Probing` / `Relayed` 状态占位

**现状**

`peer.Driver.StartPunch` 无真实打洞实现;`Probing → Relayed` 仅在打洞超时时
被触发,不产生真实中继流量。

**建议方向**

Phase 2 实现 STUN/ICE 打洞与中继转发后,这两个状态才代表真实可达路径。
详见 AGENTS.md §8.9.3。

---

## 5. 数据面明文回调 `OnPlaintext` 与 Ingress 并存

**现状**

Phase 1b 起,`core/node` 在装配了 Ingress 时会用 Ingress 接管明文投递
(`Deliver`),`OnPlaintext` 仅在**未装配 Ingress** 时使用(测试与纯数据面场景)。
两条路径并存,由 `Options.Ingress` / `Options.NoIngress` 决定。

**建议方向**

Phase 3 的 no-tun 模式接入后,`OnPlaintext` 可彻底移除,统一为 Ingress。
在此之前保留它是因为 Phase 1a 的加密数据面 E2E 测试依赖它,且它与 Ingress
互斥,不会同时生效。

---

## 6. ✅ 已关闭:单点路由已由 OSPF 替换(Phase 1c)

**现状(已解决)**

Phase 1b 的硬编码单 peer 路由(`datapath.DirectPeer`)已被 `core/route` 的
OSPF 子集替换:出站包按目的虚拟 IP 解析 NodeID,查转发表选下一跳,支持
透明多跳(A → B → C)。

`DirectPeer` 仍保留作为便捷访问器(单一直接邻居场景),但数据面不再依赖它。

---

## 7. Phase 1c 采用逐跳转发而非源路由

**现状**

多跳数据采用**逐跳加密与转发**:中间节点解密本跳、按路由表选定下一跳、
再重新加密转发,`ZCPacket` 的 Src/Dst 保持端到端语义。

**影响**

- 中间节点能看到内层明文 IP 包(逐跳解密),因此多跳路径上的中间节点
  并非 E2EE 的信任边界之外 —— 这与 AGENTS.md 8.6.4"中继不可解密"的
  目标存在张力。
- 每一跳都要做一次加解密,CPU 开销随跳数线性增长。

**原因**

Phase 1c 尚无 A ↔ C 的端到端握手(A 只与直接邻居 B 握手),因此没有
可用于端到端加密的共享会话。逐跳转发是当前拓扑下的最小可行实现。

**建议方向**

Phase 2/5 引入源路由或逐跳密钥协商:源节点与目的节点建立端到端会话,
中间节点仅按 DstNodeID 转发密文(不解密)。届时 `sendViaHop` 改为
"用端到端会话加密 + 按下一跳发送"。

---

## 8. Phase 1c Cost 仅计跳数 + RTT

**现状**

链路度量 `route.Cost(rtt) = HopCost(10) + RTT_ms`,未计入丢包率。
`latency` 策略已接收 `lossRate` 参数并在 `Weight` 中叠加 `loss×1000`,
但 `Prober` 的丢包率尚未回填到 LSA 的 Cost。

**建议方向**

Phase 5 的 weighted/latency 策略完善时,把丢包率纳入 LSA Cost:
`cost = hop_cost + RTT_ms + loss_rate × 1000`(AGENTS.md 8.6)。

---

## 9. Admin API 配置来源追踪为最佳努力

**现状**

`GET /api/v1/config/sources` 与 `mesh config sources` 只能确定"来自文件"
或"来自默认值",无法区分详细层级(cli-block / env / cli / 具体行号),
因为 `core/config` 尚未逐字段记录来源(`loader.go` 合并后只保留最终值)。

**影响**

排错时无法精确回答"这个字段为什么是这个值"——这正是 AGENTS.md §10.7
要求的能力。

**建议方向**

在 `config.Load` 中为每个字段记录来源(koanf 支持来源追踪,或合并时旁挂
一张 `map[path]source`)。这是一次需要改动配置加载核心的工作,建议在
Phase 1e/2 单独安排。

---

## 10. Admin API 未来可评估 gRPC

**现状**

Admin API 基于 HTTP + JSON(`core/api`)。CLI 与 API 共享 `core/api` 的
快照类型,类型安全由 Go 侧保证,但跨语言/跨进程契约是松散的 JSON。

**建议方向**

Phase 5 评估引入 gRPC(protobuf):性能更好、契约强类型、便于 Web 面板与
第三方集成。当前不引入以保持依赖最小(AGENTS.md 7.1 纯 Go 优先)。

---

## 11. `mesh config diff` 尚未与运行中节点对比

**现状**

`mesh config diff` 目前只输出文件层加载结果,未与运行中节点的生效配置做
真实差异对比(需要把运行配置通过 API 拉回并与文件合并结果逐字段比较)。

**建议方向**

Phase 2 热重载完善后,基于 `admin` 的 `/config` 端点实现真实 diff,并标注
每项的响应级别(hot / reconnect / restart)。
