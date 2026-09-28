# 交互式多会话设计方案（DESIGN_SESSIONS）

> 状态：方案设计（v2.0，待评审后实施）
> 关联需求：每个用户可同时保有多个**交互式**终端会话；登录后可实时查看会话列表并选择进入；支持用户自定时长、申请延期、手动关闭；长期会话在页面/网络断开后继续保留。
> 关联模块：`handler/*`、`auth/*`、`pty/*`、`sandbox/*`、`static/*`
>
> **v2.0 主要修订**（相对 v1.2）：
> 1. **重新定位**：本文只负责**交互式会话**；非交互**后台批处理任务**改由 **PBS（Altair PBS Professional）** 调度，另见 `DESIGN_JOBS.md`（待建，取代 `DESIGN_TASK.md` 的自建执行器思路）。
> 2. **新增持久化**：交互长会话需跨 webshell 服务重启/发版存活（§6，会话守护进程方案，分阶段）。
> 3. **统一安全模型**：`resume` 不再以 `sessionID+username` 作为 bearer；所有终端接入（新建/恢复）一律 `Bearer mgmt_token` 换取**一次性 ticket** 后走 WS（§4）。
> 4. 存储抽象拆分为 **本地会话管理器 + 可外置索引**（§5），修正 v1.2 接口泄漏 `*pty.Session`/`*hubSession` 的问题。
> 5. 命名/资源治理/锁层级/登出等评审意见一并修订（§7、§8、§10）。

---

## 1. 目标与定位

### 1.1 要解决的问题

当前交互式 pty 会话（`handler/session_hub.go`）只有「单会话 + 固定 5 分钟空闲保活」：

| 场景 | 现状 |
|------|------|
| 关闭页面 / 断网 | 仅 `detach`，进程保留，`expireAt` 重置为 5 分钟后 |
| 5 分钟内重连 | 同 sessionID 恢复，任务不中断 |
| 超过 5 分钟不回来 | `reaper` 回收 → `sess.Close()` → Kill 进程树 |
| 一次登录建多个会话 | 一次性 token 仅一枚，只能建一个 |
| 换电脑登录 | 无会话列表，只能靠浏览器 `sessionStorage` 恢复 |
| 服务重启 / 发版 | 进程内会话全部丢失 |

### 1.2 设计目标

1. 每个用户可同时保有 **最多 3 个长期会话**（默认 24h，用户可自定，单次最长 168h）。
2. 短期会话（5 分钟空闲）**不占**长期名额，保留现有轻量行为。
3. 登录后提供**实时会话列表**，可进入、延期、关闭。
4. 长期会话采用**绝对期限**语义：到点即 kill（无论是否在线），临近到期给出提示。
5. 支持**申请延期**（在原到期时间上累加，单次受上限约束，累计不设上限）。
6. 交互长会话**尽量跨 webshell 重启存活**（§6，分阶段）。
7. 存储抽象分层，为未来多实例 / 外置索引预留扩展点（§5、§12）。

### 1.3 范围界定（重要）

| 属于本文（交互式） | 不属于本文（批处理） |
|---|---|
| shell / opencode / codex 等**有人交互**的终端 | 非交互命令、训练/编译等**批处理** |
| 执行在**登录节点**（webshell 持有的 pty，沙箱隔离） | 执行在**PBS 计算节点**（调度器分配） |
| 生命周期由 webshell/sessiond 持有 | 生命周期由 **PBS** 持有（qsub/qstat/qdel） |
| 登录后查「会话列表」 | 登录后查「作业列表」 |

> **后台任务不要自建执行器**：直接集成集群现有的 PBS 做提交/查询/日志/取消。相关设计另立 `DESIGN_JOBS.md`（身份经 `sudo -u <user>` 运行 qsub/qstat/qdel；脚本经 stdin 传入；作业由 PBS 隔离与配额，不经 webshell 沙箱）。PBS 天然解决批处理跨重启的持久性，因此 **PBS 持久性 ≠ 交互会话持久性**，交互会话仍需 §6 的方案。

---

## 2. 术语

| 术语 | 含义 |
|---|---|
| 长期会话（long） | 用户指定时长的交互会话；占长期名额；可延期 |
| 短期会话（short，缺省） | 空闲 5 分钟回收的轻量会话；不占名额；不可延期 |
| mgmt_token | 登录后签发的、可重复使用的用户级管理凭证（HTTP `Authorization: Bearer`） |
| ticket | 由 mgmt_token 换取的、**一次性**短时凭证，用于建立 WebSocket |
| sessionID | 会话标识（非机密，仅作 ID）；不再单独作为接入凭证 |
| sessiond | 会话守护进程（§6），持有 pty 与子进程，跨 webshell 重启存活 |

---

## 3. 会话模型

### 3.1 两类会话

| 类型 | 期限语义 | 是否占名额 | 是否可延期 |
|---|---|---|---|
| **长期会话** | **绝对期限**：创建时 `expireAt = now + hours`；`detach` 不重置；到点即 kill（在线也 kill） | 是（`max_long_sessions`，默认 3） | 是 |
| **短期会话** | **空闲保活**：`detach` 时 `expireAt = now + 5min`；有连接则不回收 | 否 | 否 |

> 长期会话在 `Register` 时即写入 `expireAt = now + hours`；短期会话在首次 `detach` 时确定 `expireAt`。

### 3.2 会话状态

```go
type localSession struct {
    id        string
    username  string
    mode      string    // bash | opencode | codex
    longLived bool
    createdAt time.Time
    expireAt  time.Time // 长期=绝对到期；短期=空闲超时
    // 以下为本节点运行时资源，不参与任何序列化
    proc *pty.Session
    mu       sync.Mutex
    closed   bool
    conn     *attachedConn
    pending  []byte
}
```

### 3.3 attach / detach / 到期

- `attach`：沿用现有「踢旧连接」语义（同一会话仅一个连接）。
- `detach`：长期**不动** `expireAt`；短期 `expireAt = now + 5min`。
- 到期：见 §7 回收逻辑。

### 3.4 计数与上限（含资源治理）

- `LongCount(username) < max_long_sessions` 才可新建长期会话；在 `Register` 内**二次校验**（消除与预检竞态），超额返回 `ErrLongLimit`。
- **新增总资源上限** `max_total_sessions`（默认 10）：长期+短期合计，防止"短期不占名额"被用来开无限会话（多标签 DoS）。超限拒绝新建短期会话（返回 `ErrTotalLimit`）。
- 关闭会话后名额立即释放。

---

## 4. 凭证与安全模型（统一）

### 4.1 凭证

| 凭证 | 签发 | TTL | 用途 | 语义 | 存放 |
|---|---|---|---|---|---|
| mgmt_token | `/api/login` | `mgmt_ttl_hours`（默认 8h） | 列表 / 新建 / 恢复 / 延期 / 关闭 / 上传（所有会话级操作） | 可重复、绑定 username | 服务端内存 `MgmtTokenStore` |
| ticket（一次性） | `/api/session/ticket`（Bearer mgmt） | 60s | 仅用于建立一次 WebSocket | 一次性、消费防重放 | 服务端内存 |
| sessionID | 建会话时生成 | 会话存活期 | 会话**标识**（非机密） | 仅作 ID；接入需 ticket | 服务端 + 前端 |

### 4.2 关键约定

- **所有接入都先经 mgmt**：新建与恢复都要先 `POST /api/session/ticket`（携带 `Authorization: Bearer <mgmt>`）换 ticket，再 `ws://.../ws?ticket=<t>`。**浏览器 WebSocket 无法设置自定义头**，故用「HTTP 换一次性 ticket → WS」模式；ticket 短时一次性，即使进入 URL/日志风险也极小。
- `sessionID` 不再是 bearer：仅凭 sessionID 无法 attach，**修复 v1.2「关闭要 mgmt、劫持不要」的颠倒**。
- 会话归属按 username：mgmt 解出 username → 服务端按 username 查询/校验归属。
- **同账号可接管该账号全部会话**（换电脑重新登录即可）：这是有意的产品行为，须在文档与 UI 显式说明——**任何一次成功登录都能 attach 该用户所有存活会话**。
- `mgmt_token` 过期后需重新登录；**已 attach 的 WS 不受影响**，但列表/新建/恢复/延期/关闭需重新登录。
- 新增 `POST /api/logout`（Bearer mgmt）吊销 mgmt_token，并可选关闭该 token 名下的会话（默认不关）。

### 4.3 前端凭据与 XSS 加固（必须）

`mgmt_token` 是 8h 的用户级 bearer，若被 XSS 窃取可完全接管该用户全部终端（= 以该用户执行命令）。缓解措施：

1. **前端依赖自托管**：xterm.js / addon-fit 目前来自 `cdn.jsdelivr.net`，改为随二进制 `//go:embed` 内嵌（与 `static/` 一并），消除 CDN 投毒/中间人窃取路径。
2. **CSP**：`default-src 'self'`，禁止内联脚本；不引入外部脚本。
3. 前端一律用 `textContent`/`createElement` 渲染动态内容（现有代码已是如此，继续保持，禁 `innerHTML` 拼用户串）。
4. 可选增强：mgmt_token 改为 **HttpOnly + Secure + SameSite=Lax Cookie**（JS 不可读），配合 CSRF token；WS/HTTP 依赖 Cookie。代价是 CSRF 防护与多标签共享，需评估。**基线方案（1-3）先行**。
5. ticket 一次性、60s，写 URL 可接受。

---

## 5. 存储抽象（分层，修正 v1.2）

v1.2 的单一 `SessionStore` 把 `*pty.Session`、`*hubSession` 写进接口，与"未来 Redis 可替换"目标矛盾。改为**两层**：

### 5.1 本地会话管理器 `SessionManager`（进程内，持有 pty）

```go
// SessionManager 持有本节点的 pty 与子进程，不可序列化，不可跨节点。
type SessionManager interface {
    Create(p CreateParams) (*localSession, error) // 内部做长期/总量上限二次校验
    GetLocal(id string) (*localSession, bool)
    Close(id string) bool
}
```

### 5.2 会话索引 `SessionIndex`（元数据，可外置）

```go
// SessionIndex 维护元数据/索引/TTL/归属，可替换为 Redis 实现。
type SessionIndex interface {
    Add(info SessionInfo) error
    Update(id string, expireAt time.Time) bool
    Remove(id string)
    Get(id string) (SessionInfo, bool)
    List(username string) []SessionInfo        // 按 createdAt 倒序
    LongCount(username string) int
    TotalCount(username string) int
    // 多节点扩展点：OwnerOf(id) (node string, ok bool)
}
```

- 配置（默认/最大时长、名额）**不放在 store**，由 `handler` 从 `auth.Config` 读取后作为参数传入。
- `memoryIndex`：内存 map + 统一 reaper。
- `redisIndex`（未来）：元数据 Hash + 按 username 的索引/ZSet + TTL；`OwnerOf` 指向持有 `localSession` 的节点（§12）。

---

## 6. 交互会话持久化（跨 webshell 重启）

**为什么必须单独解决**：PBS 只保证**批处理**作业持久，交互 pty 由 webshell 进程持有，服务重启/发版会 `sess.Close()` → kill 进程树。既然"长任务仍需交互"，就要让**会话实体脱离 webshell 服务存活**。

### 6.1 目标形态：会话守护进程 `sessiond`

```
浏览器 ──HTTP/WS──► webshell(Web 前端，可重启/多副本) ──unix socket──► sessiond(持有 pty，常驻)
                                                                        └─ sandbox-init ─ bash/opencode/codex
```

- `sessiond`：独立 systemd 单元，常驻（`Restart=always`）。**唯一**持有 pty master 与子进程；按 `sessionID` 管理本地会话。
- 本地 RPC（`/run/webshell/sessiond.sock`，`SO_PEERCRED` 校验调用方为 ease）：`Create / Attach(id) / Detach / Resize / Close / List / Pending`。
- `webshell`：变为**无状态前端**，WS 连接仅做字节中继（web ↔ sessiond stream）。
- 效果：webshell 重启后 sessiond 仍在 → 重新 `List`/`Attach` 即可，会话不丢。

### 6.2 备选：tmux/screen 作为进程宿主

- 会话以 `tmux new-session -d` 启动，webshell `tmux attach` 接入。
- 优点：实现轻。缺点：**沙箱下复杂**——tmux 需进 rootfs，且 attach 需进入同一 mount/PID ns（否则需 `nsenter`），与现有 `sandbox-init` 模型冲突较大。
- 结论：作为快速试验可接受，**不作为目标形态**。

### 6.3 分阶段落地

| 阶段 | 内容 | 跨重启存活 |
|---|---|---|
| **P1（本文档实施范围）** | 多会话 UX（长/短、列表、延期、关闭、ticket 接入） | ❌（仅跨页面/断网，TLL 内） |
| **P2** | 引入 `sessiond` + unix socket，webshell 无状态化 | ✅ |

> P1 必须在 UI/文档中**明确标注**："当前服务重启会中断会话"；P2 完成后移除该限制。
> `sessiond` 也是未来多实例的天然"会话 owner"（§12）。

---

## 7. 回收逻辑（reaper 分叉）

```
长期会话：now > expireAt                    → 回收（无论是否 attached）
短期会话：conn == nil && now > expireAt     → 回收（现有空闲超时）
```

- 到点回收附着的长期会话：向连接下发 `close`（「会话已到期」）→ `proc.Close()` kill 进程树。
- 巡检间隔 30s，实际 kill 在到期后 ≤30s。
- **临近到期提醒**（可选，P1 建议）：到期前 10/5 分钟随输出或单独消息提示，方便用户延期。
- **锁层级**（避免死锁，编码约束）：统一 **先索引锁、后会话锁**；`closeSession` 须先释放会话锁再操作索引（现实现已如此，修订后保持）。索引实现内不得在持锁时回调会话锁。

---

## 8. 接口设计

### 8.1 HTTP（会话级操作一律 `Bearer mgmt`）

| 方法 | 路径 | 凭证 | 说明 |
|---|---|---|---|
| POST | `/api/login` | - | 响应新增 `mgmt_token`、`session_ttl_hours`（默认时长）、`max_session_ttl_hours`（单次上限）、`max_long_sessions`、`max_total_sessions` |
| POST | `/api/logout` | mgmt | 吊销 mgmt_token |
| GET | `/api/sessions` | mgmt | 实时返回该用户 `SessionInfo[]` |
| POST | `/api/session/ticket` | mgmt | body: `{action:"create\|attach", session?, mode?, type?, hours?}` → `{ticket, expires_in}`；在此完成**全部校验**（归属、名额、hours 范围） |
| POST | `/api/session/extend?session=<id>&hours=<n>` | mgmt | 仅长期；`expireAt += n 小时`；`1 ≤ n ≤ max_session_ttl_hours`；返回新 `expire_at` |
| POST | `/api/session/close?session=<id>` | mgmt | 校验归属并关闭 |
| GET | `/api/resume` | mgmt | 探测会话是否可恢复（供刷新场景），返回 `{ok, mode, long_lived}`（不再用 sessionID 作凭证） |
| GET/POST | `/api/upload-check`、`/api/upload` | mgmt | 改为 mgmt + 归属校验（统一凭证） |

`SessionInfo`：

```json
{
  "id": "…",
  "mode": "bash|opencode|codex",
  "long_lived": true,
  "created_at": 1727000000,
  "expire_at": 1727086400,
  "active": false
}
```

- 长期返回绝对 `expire_at`；短期可选（见 §8.3）。

### 8.2 WebSocket

- 统一接入：`/ws?ticket=<t>`。
  - `action=create`：ticket 内含已校验的 `mode/type/hours`；服务端创建会话并 attach。
  - `action=attach`：ticket 内含已校验的 `session`；服务端附着到同一会话。
- ticket 一次性、60s；WS 建立后 ticket 失效。
- `session` 消息保持 `{id}`。
- 失败（ticket 无效/过期/越权/名额满）→ `close` 并关闭连接。

### 8.3 短期会话是否进列表

- 默认**只列长期会话**；短期会话作为「当前终端」处理（刷新恢复即可）。
- 可选：列表以折叠方式显示短期会话（标记「临时·空闲回收」）。P1 采用"默认不列"。

### 8.4 延期规则

- 仅长期可延期；已过期 / 非长期 → 400 / 404。
- `newExpireAt = oldExpireAt + hours`（原到期时间累加，提前延期不损失剩余时长）。
- 单次 `hours ≤ max_session_ttl_hours`；**累计不设上限**（资源治理见 §3.4 总名额 + 到期提醒）。

---

## 9. 前端交互

```
登录成功 → 存 mgmt_token → GET /api/sessions 渲染面板
   ├─ 会话列表（模式 / 长期·临时 / 创建时间 / 剩余时间 / 使用中·空闲）
   │     进入 → POST /api/session/ticket {action:attach,session} → /ws?ticket
   │     延期（仅长期）→ 输入小时（默认 1，max=单次上限）→ extend → 刷新
   │     关闭 → close → 刷新列表
   ├─ 新建：
   │     类型 ○长期 ●临时
   │     [长期时] 时长（小时）默认 session_ttl_hours，min 1，max max_session_ttl_hours
   │     模式 [Bash][OpenCode][Codex]
   │     → POST /api/session/ticket {action:create,...} → /ws?ticket
   │     面板显示「长期 n/3」「总会话 m/10」；达上限时置灰/提示
   └─ 重新登录 / 登出

终端工具条「会话」按钮 → 断 WS（detach，长期保活）→ GET /api/sessions 重渲染
刷新页面 → sessionStorage 有 mgmt_token → 直接实时列表（无需重登）
关闭标签 → sessionStorage 清；会话仍在服务器，重新登录可找回（同账号可接管，见 §4.2）
无会话且仅 bash：保持现状直接进入 bash
```

---

## 10. 配置

命名去掉易混淆项：数量用 `max_*_sessions`，时长用 `*_hours`。

| 配置项 | 含义 | 默认 | 备注 |
|---|---|---|---|
| `session_ttl_hours` | 新增长期会话的**默认时长** | 24 | 前端默认值 |
| `max_session_ttl_hours` | **单次**输入最大时长（新建与延期共用，即"最大增量"） | 168 | 超限拒绝 |
| `max_long_sessions` | 每用户长期会话数量上限 | 3 | |
| `max_total_sessions` | 每用户会话总数上限（长+短） | 10 | 防短期无序创建 |
| `mgmt_ttl_hours` | 管理 token 有效期 | 8 | 一个工作日 |
| `token_ttl` | （保留）一次性 token 通用 TTL（秒） | 300 | 与 ticket 60s 区分：ticket 独立常量 |
| （短期会话） | 空闲保活 | 5min | 硬编码 |

派生值（`auth.Config`）：

```go
SessionTTLd   time.Duration // session_ttl_hours
MaxAttachTTL  time.Duration // max_session_ttl_hours（单次最大增量）
MgmtTTLd      time.Duration // mgmt_ttl_hours
MaxLongN      int           // max_long_sessions
MaxTotalN     int           // max_total_sessions
```

---

## 11. 文件改动清单

| 文件 | 改动 |
|---|---|
| `auth/config.go` | 新增 `session_ttl_hours` / `max_session_ttl_hours` / `max_long_sessions` / `max_total_sessions` / `mgmt_ttl_hours` 及派生值；`ExampleConfig` 同步 |
| `config.example.yaml` | 同步上述配置项 |
| `auth/mgmt.go`（新增） | `MgmtTokenStore`：`Issue` / `Validate` / `Revoke` |
| `auth/ticket.go`（新增） | `TicketStore`：一次性、60s、携带已校验意图 |
| `auth/auth.go` | 新增 `IssueMgmt` / `ValidateMgmt` / `RevokeMgmt` |
| `handler/manager.go`（新增） | `SessionManager`（持有 pty）+ `localSession` + 迁入 attach/detach/pump/closeSession |
| `handler/index.go`（新增） | `SessionIndex` 接口 + `memoryIndex` + 分叉 reaper（统一锁层级） |
| `handler/session_api.go`（新增） | `NewSessionsHandler`(list)、`NewTicketHandler`、`NewExtendHandler`、`NewCloseHandler`、`NewLogoutHandler`；`requireMgmt` 中间件 |
| `handler/ws.go` | 改为消费 `ticket`；create/attach 分派 |
| `handler/login.go` | 响应带 `mgmt_token` 与新配置项 |
| `handler/upload.go` | 凭证改为 mgmt + 归属校验 |
| `main.go` | 建 manager/index/store，注入；注册新路由（含 `/api/logout`） |
| `static/index.html` | 内嵌 xterm（去 CDN）+ CSP；会话面板（列表 + 新建 类型/时长/模式 + 登出）；工具条「会话」按钮 |
| `static/app.js` | mgmt 存取；实时列表；ticket 换发；进入/新建/延期/关闭；`type`/`hours` 透传 |
| `static/css/style.css` | 列表与新建区样式 |
| `README.md` / `USER_GUIDE.md` | 文档同步 |

---

## 12. 未来多实例 / Redis（基于 §5、§6）

- **形态 A（粘性路由 + 外置索引）**：`webshell` 多副本无状态；`redisIndex` 存元数据/索引/TTL/owner 节点；WS 按 sessionID 一致性哈希路由到 owner 的 `sessiond`。
- **形态 B（sessiond broker 化）**：把 §6 的本地 unix socket 升级为跨节点 gRPC；`sessiond` 即"会话 broker"，每会话归属固定节点。
- 迁移要点：`OwnerOf` 路由、跨节点 attach 互斥（分布式锁）、`pending` 外置或有界丢弃、TTL 一致性、Redis 内网可达 + 认证/TLS、mgmt/ticket 存哈希。
- 服务/节点宕机仍会丢失其上的 pty（形态 C 容器编排可进一步缓解）。

---

## 13. 安全与边界

1. mgmt_token 高熵、绑定 username、每次校验；走 `Authorization` 头；不打印原文；支持登出吊销。
2. **接入一律经 mgmt 换一次性 ticket**；`sessionID` 不再是凭证（修复劫持面）。
3. mgmt 过期后管理操作需重登；已建立的 WS 不受影响。
4. 同一会话仅一个连接 attach（沿用「踢旧连接」）；多标签需 UI 提示"该会话已被占用/将被接管"。
5. **前端依赖自托管 + CSP**（消除 CDN XSS 窃取 mgmt_token 的路径）。
6. 单实例假设见 §12；服务重启的交互会话丢失由 §6 P2 解决。
7. 会话进程自然退出 → 泵结束 → 索引移除 → 列表自动反映。
8. 同账号可接管全部会话，须在 UI 明示。

---

## 14. 测试矩阵

| 场景 | 预期 |
|---|---|
| 建 1/2/3 个长期会话 | 成功 |
| 建第 4 个长期会话 | 拒绝，`close`「长期会话数量已达上限」 |
| 短期会话超过 `max_total_sessions` | 拒绝，`close`「会话总数已达上限」 |
| 新建长期 `hours=0` / 负 / 非数字 / >168 | 拒绝 |
| 新建长期 `hours=10` | `expireAt = now + 10h` |
| 新建长期缺 `hours` | 用默认 24h |
| 短期断线 | 5min 后回收 |
| 长期断线/在线到点 | 到期回收（在线发 `close`） |
| 延期 5h / >168h / 非长期或已过期 | `+=5h` / 拒绝 / 拒绝 |
| 累计多次延期 | 允许 |
| 关闭长期后新建 | 名额释放 |
| 无 / 错 / 过期 mgmt 访问各接口 | 401 |
| 仅用 sessionID（无 ticket）尝试 attach | 拒绝（401） |
| 他人 mgmt 操作他人会话 | 404 |
| ticket 重放 / 过期 / 越权 | 拒绝 |
| 登出后 mgmt 失效 | 401 |
| 恢复（刷新） | 经 ticket 正常 attach |
| 缺 `type` | 视为短期，向后兼容 |
| `memoryIndex` 单元 | 增删改查 / 排序 / 计数 / 上限 / 回收 |
| `MgmtTokenStore` / `TicketStore` 单元 | 签发 / 校验 / 过期 / 重放拒绝 |
| 并发创建抢名额 | 恰好不超额（Register 内二次校验） |
| reaper 与 attach/close 竞态 | 无死锁、无双重关闭 |

---

## 15. 实施顺序（P1）

1. 配置（`auth/config.go` / `config.example.yaml`）
2. `auth/mgmt.go` + `auth/ticket.go` + `auth/auth.go`
3. `handler/manager.go` + `handler/index.go`（分层 + reaper）
4. `handler/session_api.go`（list/ticket/extend/close/logout + requireMgmt）
5. `handler/ws.go`（ticket 接入）/ `login.go` / `upload.go` / `main.go`
6. 前端（内嵌 xterm + CSP；`index.html` / `app.js` / `css`）
7. `go build ./...` + `go test ./...`
8. 文档同步（可选）

P2（会话守护进程 `sessiond`）单独立项，见 §6.3。
