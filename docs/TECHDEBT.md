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

## 6. Phase 1b 两点直连路由是临时实现

**现状**

`datapath.DirectPeer` 要求恰好存在一个活跃会话:0 个则丢弃出站包,
多于 1 个则报"路由歧义"错误。这是 Phase 1b"两点直连"的约定。

**影响**

三个及以上节点时,出站 IP 包无法判断下一跳,`DirectPeer` 返回错误、
包被丢弃。多节点组网不可用。

**建议方向**

Phase 1c 实现 OSPF(链路状态泛洪 + Dijkstra)与路由表后,出站包按目的
虚拟 IP 查表选下一跳,`DirectPeer` 退化为"唯一直接邻居"的便捷访问器或移除。
详见 AGENTS.md §12.3。
