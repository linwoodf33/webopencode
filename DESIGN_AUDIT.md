# 终端会话审计录制设计方案（DESIGN_AUDIT）

> 状态：方案设计（关键决策已确认，见 §15，待实施；2026-10 评审修订版）
> 关联需求：记录用户在 Web 终端上的**输入/输出**，形成可回放的「录屏」，用于事后审计（谁、何时、从哪、在哪个会话里做了什么）。
> 关联模块：`handler/*`、`pty/*`、`auth/*`、`static/*`、`main.go`
> 关联文档：`DESIGN_SESSIONS.md`（会话模型/锁层级/凭证）、`DESIGN_TASK.md`（批处理任务，暂不在本文范围）

---

## 1. 目标与定位

### 1.1 要解决的问题

当前所有终端 I/O 都是**瞬态**的：`handler/manager.go` 的 `pump`→`sendOutput` 只把 pty 字节实时推给当前连接，断开即丢；`pending` 缓冲上限 1MB 且重连后冲刷完即清；没有任何落盘。因此事后无法回答：

| 审计问题 | 现状 |
|---|---|
| 某用户在 3 天前执行过什么命令 | 无记录 |
| 某次可疑操作的完整过程 | 只能看当时的实时屏幕，事后丢失 |
| 操作来自哪个 IP / 哪次登录 | `ws.go` Upgrade 时未采集来源 |
| 跨断线重连的完整时间线 | `pending` 冲刷后不留痕 |

### 1.2 设计目标

1. **完整录制**每个会话的 output、input、resize，带**真实时间戳**（不做空闲压缩），可离线回放为「录屏」。
2. **录制与连接解耦**：长会话在 detach（关页面/断网）期间的后台输出同样被录制（埋点放在 `sendOutput`，而非仅在附着时）。
3. 录制**绝不阻断终端**：写盘失败/磁盘满时降级为丢弃并计数，不影响交互。
4. 提供**只读回放**页面（播放/暂停/倍速/进度/跳转）。
5. **权限分层**：用户本人可看自己；**审计员/管理员**可查看全部（新增角色概念）。
6. 满足取证诉求：记录**来源 IP、User-Agent、会话元数据、attach/detach/close 时间线**。
7. 磁盘可控：单会话上限 + 保留期 + 总量配额，自动清理。

### 1.3 范围界定

| 属于本文 | 不属于本文 |
|---|---|
| 交互式 pty 会话（bash/opencode/codex）的 I/O 录制 | 批处理任务（`DESIGN_TASK.md`，若落地另接） |
| 终端字节流级回放 | 像素级屏幕录像（不做，性价比低、隐私面大） |
| 用户/管理员查看 | 实时屏幕直播（不做，见 §15） |

---

## 2. 术语

| 术语 | 含义 |
|---|---|
| recorder | 单个会话的录制器，持有该会话的 cast 文件与元数据 |
| cast | 录制数据文件，采用 **asciicast v2**（NDJSON）格式 |
| 事件（event） | cast 中的一行：`[time, type, data]`，type ∈ `o`(output)/`i`(input)/`r`(resize) |
| audit_dir | 录制文件根目录（配置项） |
| 审计员 | 被授权查看**他人**录屏的用户（见 §7），由配置白名单或 AD 组界定 |

---

## 3. 录制模型与格式

### 3.1 为什么用 asciicast v2

- **行业标准**（asciinema），可直接用 `asciinema play`、asciinema-player、第三方取证工具打开，避免自造格式的移植成本。
- NDJSON 追加写，天然支持**流式写入**与**崩溃后残留可读**（已落盘的行仍可播放）。
- 与现有 `protocol.Message` 的 output 语义（原始字节 + 转义序列）一致，无需二次编码。

### 3.2 事件流

首行 header（JSON），其后每行一个数组事件：

```json
{"version":2,"width":80,"height":24,"timestamp":1727000000,
 "env":{"SHELL":"/bin/bash","TERM":"xterm-256color"},
 "title":"alice@host: bash"}
[0.248848, "o", "\u001b]0;alice@host\u0007"]
[1.001376, "i", "ls -la\n"]
[1.540000, "r", "120x30"]
[2.120000, "o", "total 12\r\n"]
```

- `time`：相对会话开始的**墙钟秒（float）**，不做空闲折叠；回放器可自行提供「跳过空闲」。header **省略 `idle_time_limit` 字段**（不写该字段，避免被 player 解释为「0 秒 idle 上限」）。
- `o`：pty 输出原始字节（UTF-8 / 转义序列），与 `protocol.OutputMessage` 同源；写入前经**跨块 UTF-8 重组**（见下方说明）。
- `i`：客户端发往 pty 的输入字节（见 §3.4 / §11 隐私）。
- `r`：resize，`"<cols>x<rows>"`。
- header 的 `timestamp` 为会话绝对起始时刻，`width/height` 初值 80×24，**首个 `r` 事件到达后以真实尺寸为准**（回放器优先用 `r`）。
- header `env` **白名单仅 `SHELL`/`TERM`**，绝不含其他环境变量（防 api_key 类敏感值进 header）。

> **header 延后落盘**：`recorder` 在收到第一个事件（或 1s 超时）前不写 header，以便 header 携带已解析出的初始尺寸；等待期间事件在内存排队（有界，默认 4KB），超限则先以 80×24 写 header 并冲刷。

> **UTF-8 保真与块边界重组**：pty 输出是字节流，`pty.Read`（4096 字节块）可能在**多字节字符中间截断**，且终端输出不保证 UTF-8（如 GBK 文件名/内容）。因此：
> 1. recorder 维护**跨块 UTF-8 重组缓冲**：不完整尾序列留待下一块拼接后再编码，消除块边界假 `\ufffd`；
> 2. 重组后仍无效的字节按 Go `json.Marshal` 语义替换为 `U+FFFD`（**有损**，属明示取舍：保证 cast 每行仍为合法 JSON；如需字节级保真，P5 可引入 base64 扩展字段，偏离标准、不默认）。

### 3.3 审计扩展信息

asciicast 无「注释」能力，故**审计元数据与时间线单独落 `meta.json`**，保持 `cast.log` 纯标准（可移植）：

```
<audit_dir>/<username>/<sessionID>/
├── meta.json     # 会话元数据 + attach/detach/close 时间线（JSON）
└── cast.log      # asciicast v2 事件流（NDJSON）
```

`meta.json`：

```json
{
  "session_id": "…",
  "username": "alice",
  "mode": "bash|opencode|codex",
  "long_lived": true,
  "created_at": 1727000000,
  "expire_at": 1727086400,
  "started_at": 1727000000,
  "ended_at": 1727003600,
  "end_reason": "session expired|shell exited|session closed|server shutdown",
  "client": {"ip": "10.1.2.3", "forwarded_for": "", "user_agent": "Mozilla/5.0 …"},
  "timeline": [
    {"t": 1727000000, "event": "open"},
    {"t": 1727000010, "event": "attach", "client": {"ip": "10.1.2.3"}},
    {"t": 1727000080, "event": "upload", "file": "report.csv", "dest": "/home/alice/report.csv"},
    {"t": 1727000120, "event": "detach"},
    {"t": 1727003600, "event": "close", "reason": "session expired"}
  ]
}
```

- `open/attach/detach/close` 记录在 `SessionManager` 的 Attach/Detach/close 埋点（§4）。
- **管理事件亦入 timeline**：`upload`（上传不经 pty，需在 `handler/upload.go` 显式补记）、`extend`/`close-req`（`handler/session_api.go`）、`auth-provided`（仅记变量名清单，**不含 key 内容**；发生在 rec.Open 之前，由 ws.go 暂存、Create 时随 meta 落盘）。
- `client.ip` 取 Upgrade 时的 `r.RemoteAddr`；反代场景可选信任 `X-Forwarded-For`（§5，默认关闭）。

### 3.4 输入脱敏（依据终端 ECHO 信号）

**问题**：终端在密码提示下「看不见输入」，是内核 line discipline 依据 pty 的 `ECHO` 标志不把输入回显到 master（因此不进 output）。但浏览器发来的输入字节仍原样写入 pty，若直接录制会**明文落盘密码**。

**方案：用内核 `ECHO` 标志作为权威脱敏信号**。判据与内核决定「是否回显」用的是同一个标志，因此能精确对齐终端行为：

- 录制输入前，读取 pty master 的 termios（`c_lflag`）；`ECHO=0` 即「终端此刻不显示输入」。
- **仅当 `ECHO=0 && ICANON=1` 时脱敏**（`password_input.mask_when: echo_off_canonical`，默认）：
  - `getpass()`/`sudo`/`ssh`/`passwd` 等密码提示均为「关 echo、保留行缓冲（ICANON）」——精确命中；
  - **raw 模式 TUI**（opencode/codex/vim/less 等，`ICANON=0`）不属于密码场景，**不脱敏**，正常录入审计，避免误伤。
  - 可选严格模式 `echo_off`（只要 `ECHO=0` 即脱敏），覆盖极少数关 echo 且关 ICANON 的密码程序。

**处理策略**（配置 `password_input`，默认 `mask`）：

| 取值 | 行为 | 对齐终端 |
|---|---|---|
| `mask`（默认） | 该输入块不记内容，记等长掩码占位（如按字节数替换为 `*`，`i` 事件 data 为掩码） | 视觉上「已输入但不可见」 |
| `omit` | 完全不写该输入块（最贴近终端——终端什么都不显示） | 是 |
| `plaintext` | 原样录制（合规需要可恢复密码时的逃生口，需显式开启并提示风险） | 否 |

- 脱敏按**输入块**判定：浏览器逐键发送，`readLoop` 每次收到输入时即时查询 `EchoState()`，因此密码逐键均被脱敏；提示符结束、程序恢复 `ECHO` 后立即停止脱敏。
- 掩码保留**按键次数与节奏**（审计可推断"输入了 N 个字符"），但不含内容；同时在 `meta.json` 记录 `masked_input_events` 计数，供审计快速定位。
- **对回放的影响**：`i` 事件内容被掩码即天然不可恢复；「显示输入」的可行性以 §8.2 spike 结论为准（player 很可能忽略 `i`，需自建输入面板），无论如何掩码内容不泄露；配合「回放侧默认不显示 input」，双重保护。
- **api_key 无关**：`auth-request/auth-response` 不经 pty（§4），本机制与其互补，覆盖交互 shell 内的密码提示。

**实现**（新增 `pty.Session.EchoState`，`golang.org/x/sys/unix` 已是直接依赖，`sandbox/sandbox.go:36` 已在用）：

```go
// pty/session.go
import "golang.org/x/sys/unix"

// EchoState 返回 pty 当前的回显与规范模式状态（读取 master 的 termios c_lflag）。
func (s *Session) EchoState() (echo, canonical bool, err error) {
    s.mu.Lock()
    master, closed := s.master, s.closed
    s.mu.Unlock()
    if closed || master == nil {
        return false, false, os.ErrClosed
    }
    t, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
    if err != nil {
        return false, false, err
    }
    return t.Lflag&unix.ECHO != 0, t.Lflag&unix.ICANON != 0, nil
}
```

`readLoop` 埋点改为：查 `EchoState()` → 命中脱敏条件则 `rec.WriteInputMasked(len(input))`，否则 `rec.WriteInput(input)`。termios 查询为轻量 ioctl，在无锁、写 pty 前完成，不影响既有锁层级（R1–R4）。

> **边界说明**：若程序自行打印 `*`（少数实现），输出侧本就显示星号，与本机制无冲突；`ECHO=0` 期间程序主动回显原文属异常实现，不在覆盖范围。输入块与 echo 状态切换的竞态窗口极小（最多丢失/多记最后一个 `\n`，非敏感）。**已知盲区**：raw 模式 TUI（`ICANON=0`）内的密码/密钥输入**不脱敏**（如 TUI 内嵌表单），默认判据不覆盖；需更严格时用 `password_mask_when=echo_off`（§10）。

---

## 4. 采集点（后端埋点）

录制器由 `SessionManager` 在 **Create** 时挂到 `localSession`，随会话生命周期存在；所有埋点均在**锁外**（遵守既有 R1–R4 与「不持锁做 I/O」约束）。

| 时机 | 位置 | 动作 |
|---|---|---|
| 会话创建 | `SessionManager.Create`（`handler/manager.go:212`） | `recorder.Open(id, username, mode, clientInfo)`，写 `meta.json`，记 `open` |
| pty 输出 | `localSession.sendOutput`（`handler/manager.go:66`）**入口** | `rec.WriteOutput(data)` —— 无论是否附着都录（含 detach 期后台输出） |
| 客户端输入 | `localSession.readLoop` TypeInput（`handler/manager.go:112`） | 查 `proc.EchoState()` 判定脱敏（§3.4）：命中则 `rec.WriteInputMasked(len)`，否则 `rec.WriteInput(input)` |
| 窗口尺寸 | `localSession.readLoop` TypeResize（`handler/manager.go:120`） | `rec.WriteResize(cols,rows)` |
| 附着 | `SessionManager.Attach`（`handler/manager.go:259`） | 记 `attach` + 来源 IP |
| 分离 | `SessionManager.Detach`（`handler/manager.go:308`） | 记 `detach` |
| 终结 | `SessionManager.close`（`handler/manager.go:343`） | 记 `close(reason)`；`rec.Close()` 置于**步骤⑤（proc.Close）之后**（此时 pump 已因 EOF 退出，无并发写），flush + 落 `ended_at` |
| 上传 | `NewUploadHandler` 成功后（`handler/upload.go`） | `rec.Event("upload", {file, dest})`——上传不经 pty，需显式补记 |
| 延期/关闭请求 | `NewExtendHandler` / `NewCloseHandler`（`handler/session_api.go`） | `rec.Event("extend" / "close-req", …)` 入时间线 |

**关键设计**：

1. **output 埋点在 `sendOutput` 而非 `writeTo`**：`writeTo` 仅在附着时调用，会漏掉 detach 期间的后台输出；`sendOutput` 是唯一汇聚点（`pump` 每读一段即调）。
2. **不阻塞终端**：`recorder` 内部维护 `bufio.Writer` + 有界内存队列；写操作仅 append 到内存，由**独立 goroutine** 周期 flush 或按阈值 flush。队列满（磁盘慢/满）时**丢弃并计数**，绝不阻塞 `pump`/`readLoop`。丢弃数写入 `meta.json` 的 `dropped_events`。
3. **api_key 不落录**：`auth-request/auth-response` 流程在 `localSession` 创建**之前**直接读写 conn（`handler/ws.go:126`、`handler/auth_flow.go`），**不经过 pty**，因此天然不进入 `cast.log`。设计上明确保持这一隔离。
4. **rec.Open 失败语义（默认 fail-open）**：磁盘/IO 错误导致 recorder 打不开时，**照常开会话**（与目标 3 一致），仅告警日志 + 计数；合规更强要求可配置 `audit.fail_closed: true` 改为拒绝建会话（§10）。
5. **Create 失败路径清理**：`index.Add` 超额等失败时，若 recorder 已 Open，先 `rec.Close("session rejected")` 再返回，避免残留无主目录。
6. **rec.Close 时序与并发契约**：置于 close 六步的**步骤⑤之后**；recorder 保证 **Close 后写入安全丢弃、Close 幂等、多 goroutine 并发安全**（pump/readLoop 与 close 并发，见 §4.1）。

### 4.1 recorder 接口（新增 `audit` 包）

```go
// audit/recorder.go
//
// 并发契约：Write* 可被 pump/readLoop 并发调用；Close 幂等，且 Close 后的
// 写入安全丢弃（不 panic、不损坏文件）。所有方法不阻塞调用方（内部有界队列）。
type Recorder struct { /* bufio.Writer, mu, queue, counters, meta */ }

func Open(dir string, info SessionMeta) (*Recorder, error)   // 失败由调用方决定 fail-open/closed
func (r *Recorder) WriteOutput(b []byte)                     // 内部跨块 UTF-8 重组（§3.2）
func (r *Recorder) WriteInput(b []byte)
func (r *Recorder) WriteInputMasked(n int) // 脱敏：记等长掩码，不记内容（§3.4）
func (r *Recorder) WriteResize(cols, rows uint16)
func (r *Recorder) Event(name string, fields map[string]any) // timeline；meta.json 原子重写（temp+rename）
func (r *Recorder) Close(reason string)                      // flush + ended_at；幂等
```

`SessionManager` 新增字段 `recFactory func(SessionMeta) (*audit.Recorder, error)`（可注入，单测替换为内存桩），与现有 `closeProc` 接缝风格一致（`handler/manager.go:154`）。

---

## 5. 来源信息（IP / UA）

在 `handler/ws.go` Upgrade 前采集并随 ticket 意图或 Create/Attach 调用传入：

```go
type ClientInfo struct { IP, ForwardedFor, UserAgent string }
```

- 默认 `IP = 远端地址（r.RemoteAddr 去端口）`。
- 反代场景：仅当配置 `audit.trust_proxy: true` 时采纳 `X-Forwarded-For` 首个地址（防伪造，默认关闭）。
- `attach` 的来源 IP 由 WS 连接提供；`create` 的在 Create 时记录。

---

## 6. 存储与保留

### 6.1 布局与权限

- 根目录 `audit_dir`（默认 `/var/log/webshell/audit`），服务用户可写（或 root 预建 + 属主）。
- 目录/文件权限 `0700` / `0600`；文件属主为服务用户（`ease`）。
- 路径：`<audit_dir>/<username>/<sessionID>/{meta.json,cast.log}`。

### 6.2 上限与清理

| 策略 | 说明 |
|---|---|
| 单会话上限 | `max_session_bytes`（默认 100MB）；超限停止记录 output 并计数（保留前段，保证不撑爆磁盘），`meta.json` 记 `stopped_recording: true` |
| 保留期 | `retention_days`（默认 90）；按 **`ended_at`** 判定过期；后台清扫任务删除过期会话目录 |
| 总量配额 | `max_total_bytes`（默认 20GB）；超限按最旧优先删除 |
| 人工删除 | 不支持，审计数据只读，仅保留期/配额自动清理 |

清扫由**独立 goroutine ticker**（默认 1h）驱动，与 `memoryIndex.runReaper`（`handler/index.go:292`）同风格；启动时也跑一次。**清扫必须排除当前活跃会话**（recorder 尚未 `Close` 的会话清单由 `SessionManager` 提供），避免删除正在写入的录制。

### 6.3 残留 cast 收尾（服务重启）

会话实体本身不跨 webshell 重启存活（`DESIGN_SESSIONS.md` §6 P2），但 `cast.log`/`meta.json` 已落盘。为让重启后每条录屏都有**明确结束态**（审计要求，避免"永不结束"的悬空记录）：

1. **优雅停机**（正常发版/重启）：`main.go` 新增 `http.Server` + `signal.NotifyContext(SIGINT/SIGTERM)`，收到信号后**先关会话、再关 HTTP**（顺序保证确定性落盘）：
   - 遍历 `SessionManager` 现存会话，逐个 `Close("server shutdown")` → 关闭 WS/pty 并触发 `rec.Close("server shutdown")`（flush + 写 `ended_at`/`end_reason`）；
   - 再 `srv.Shutdown(ctx)`（带超时，默认 15s）兜底停止 HTTP（gorilla WS 属 hijacked 连接，`Shutdown` 不等待，但先关会话可确保录屏收尾一定执行）；
   - 停止 audit 清扫 goroutine。
2. **崩溃/强杀残留**（未走优雅停机）：进程下次启动时，由 `audit.store` 做一次**启动巡检**——扫描 `audit_dir` 下所有会话目录，凡 `meta.json` 缺少 `ended_at`（或缺失 `end_reason`）者，视为上次异常终止，补写：
   - `ended_at` = 该目录 `cast.log` 最后一个事件时间 + `started_at`（无事件则用文件 mtime）；
   - `end_reason` = `"server restart (abnormal)"`；
   - 在 `timeline` 追加 `{"t": now, "event": "close", "reason": "server restart (abnormal)"}`。
   - `meta.json` 一律**原子写**（temp 文件 + `rename`），崩溃损坏的 meta 按「缺 `ended_at`」容错补写；
   - 巡检对 `cast.log` 只读，不重放，幂等（已有 `ended_at` 的目录直接跳过）。

> 说明：因交互会话不跨重启存活，重启本身即"会话终止"的权威信号；上述补写仅补齐审计所需的**结束标记与时间戳**，不尝试恢复会话。
> **丢失窗口披露**：录制为异步落盘（内存队列 + 周期 flush），进程被 `kill -9` 时队列中未 flush 的尾部数据丢失，丢失上限 ≈ flush 周期（目标 ≤1s）；优雅停机路径无此问题（Close 前强制 flush）。该窗口供合规评估，需更强保证可缩短 flush 周期或改同步写。

---

## 7. 权限模型（新增审计员角色）

当前 `auth` 只有 AD 认证 + 系统用户，无角色（`handler/session_api.go:73` 的 `RequireMgmt` 仅解析 username）。审计需要「看他人」的能力，新增：

### 7.1 审计员判定

配置 `audit.admins: ["alice", "auditor"]`（用户名白名单，最小改动）。判定 `IsAuditor(username) bool`。
> 进阶：AD 组（如 `CN=WebShell-Auditors`）作为第二来源；起步用白名单。

### 7.2 访问规则

| 端点 | 普通用户 | 审计员 |
|---|---|---|
| 列举**本人**录屏 | ✅ | ✅ |
| 列举**全部/指定用户**录屏 | ❌ | ✅ |
| 回放本人录屏 | ✅ | ✅ |
| 回放他人录屏 | ❌ | ✅ |
| 删除录屏 | ❌ | ❌ |

- **审计数据只读**：不提供任何人工删除入口（无 delete 端点、无前端删除按钮）；数据仅由保留期/总量配额自动清理（§6.2）。`active`（进行中）会话不可回放/下载（cast/download 返回 `409`），但 `meta` 仍可读以展示状态。
- 所有审计 API 仍走既有 `RequireMgmt`（Bearer mgmt_token，`handler/session_api.go:73`）；在其上叠加 `RequireAuditor`（校验 username ∈ admins）用于越权端点。
- 归属判定：普通用户强制 `target==self`；审计员可显式指定 `username`（缺省全部）。
- 越权一律 `404`（防枚举），与既有会话接口一致（`handler/session_api.go:175`）。
- `/api/login` 响应新增 `is_auditor` 字段（`handler/login.go`），供前端决定是否展示审计入口；权限判定以服务端为准，前端仅作展示控制。

### 7.3 用户本人可见性

配置 `audit.user_can_view_self`（默认 true）。关闭后仅审计员可看。

---

## 8. 回放前端

### 8.1 形态

在现有 UI 增加 **审计(Audit)** 入口（工具栏），进入：
- **审计员**：用户筛选 + 会话列表（用户 / 模式 / 起止时间 / 时长 / 来源 IP / 大小）。
- **普通用户**：仅本人列表。
- 点开一条 → **只读回放页**。

### 8.2 回放器实现（引入 asciinema-player，自托管）

回放采用 **asciinema-player**。其原生消费 asciicast v2（与 §3 录制格式同源），播放/暂停/倍速/进度/全屏/快捷键/`startAt` 开箱即用，避免自写回放器的状态机与 seek 复杂度。与现有前端依赖一致的强制约束：**必须自托管**（`//go:embed` 内嵌，禁止 CDN，满足 CSP `script-src 'self'`，`main.go:57`、`DESIGN_SESSIONS.md` §4.3）：

- 新增 `static/vendor/asciinema-player.js` 与 `static/vendor/asciinema-player.css`（来源 npm `asciinema-player`）；比照 `DESIGN_SESSIONS.md` §16 的 vendor 校验流程记录版本 + SHA-256，并去除官方文件末尾的 `sourceMappingURL` 注释。
- CSP：脚本面仍 `script-src 'self'`；player 运行时若动态注入 `<style>`，现有 `style-src 'self' 'unsafe-inline'`（`main.go:57`，为 vendored xterm 已放行）足以覆盖；样式优先随 `.css` 静态引入。**不得**为 player 放宽脚本面。
- 回放页按需加载 player（仅在打开审计回放时加载），不影响主终端页首屏。

数据通路：

```
loadCast(session) → GET /api/audit/cast?session=… 返回 asciicast NDJSON 原文
  → 前端取得 cast 文本（{data} 内联）或同源 URL
  → AsciinemaPlayer.create(src, container, { speed, startAt, autoPlay, rows, cols })
    （API 形态以实际引入的 player 版本为准，v3 为 AsciinemaPlayer.create(src, el, opts)）
```

- **`i`（input）事件支持需 spike 验证**：web 版 asciinema-player 很可能只渲染 `o`/`m`/`r`、直接忽略 `i`（此前「输入叠加配置」的说法存疑）。落地 P3 前先做 spike；若确认忽略，则「显示输入」改为**自建输入面板**——从 cast 解析 `i` 事件，在播放器旁按时间轴对齐渲染，默认隐藏、审计员显式开启。无论何种形态，**回放侧默认不显示 input**（§11）不变。
- resize 事件由 player 依据 `r` 自适应；header 的 `width/height` 仅作初值。
- **加载方式（CSP 约束）**：`connect-src 'self'` **不含 `blob:`**，禁止前端用 Blob URL 交给 player（其内部 fetch 会被拦截）。统一使用**同源 URL 直载**（`/api/audit/cast?...`，`AsciinemaPlayer.create({url}, …)`）或 `{data: <cast 文本>}` 内联模式。
- **大文件**：P0 整段加载；超阈值（如 >20MB）时 P2 引入服务端预处理（关键帧 + 分段，配合 §9 的 `t_from/t_to` 与 `startAt`）。

### 8.3 明示录制

登录后首次进入终端 / 打开审计页时，UI 明示「本会话的输入输出将被记录用于审计」，并说明**终端所见（output）将原样留存、密码输入按终端行为脱敏**（合规要求，见 §11）。

---

## 9. API（新增，全部 `Bearer mgmt`）

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/audit/sessions?username=&from_date=&to_date=&offset=&limit=` | self / auditor | 录屏会话列表（按 created_at 倒序）；普通用户 `username` 强制为本人。日期参数命名 `from_date/to_date`，与 cast 的相对秒参数区分 |
| GET | `/api/audit/meta?session=` | self / auditor | 取 `meta.json`（元数据 + 时间线）；**对 active 会话仍可读** |
| GET | `/api/audit/cast?session=&t_from=&t_to=` | self / auditor | 取 cast 事件；`t_from/t_to` 为**相对秒**切片，支持大文件分段；**活跃会话（未 Close）返回 `409`** |
| GET | `/api/audit/download?session=` | auditor | 打包 `meta.json`+`cast.log`（zip）下载取证（`cast.log` 本身即标准 `.cast`，可直接供 asciinema 工具链使用）；**活跃会话（未 Close）返回 `409`** |

- **无删除端点**：审计数据只读，不提供人工删除能力；清理仅由保留期/总量配额自动执行（§6.2）。
- **active 会话语义**：`active`（未 Close）会话无完整回放/取证价值，故 `/api/audit/cast` 与 `/api/audit/download` 直接返回 `409 {"error":"active session"}`（在写任何响应体/响应头之前判定）；`/api/audit/meta` 不受影响，保持可读。

- `/api/audit/cast` 默认**原样返回 asciicast NDJSON**（`Content-Type: application/x-asciicast`），同源 URL 可直接交给 asciinema-player 加载，无需前端二次解析。
- 需要时间窗口/大文件分段时，服务端按 `t_from/t_to` 过滤事件，**并将事件 time 平移 `-t_from`**（窗口内时间从 0 起，`startAt=0` 可播）；可附带窗口起点前最近的 `r` 事件以恢复渲染基准（P2 优化项）。前端以 `{data: <返回文本>}` 内联模式交由 player 播放（**不用 Blob URL**，见 §8.2 CSP 约束）。
- 时间过滤在服务端做（避免前端拉全量），依赖事件时间为相对秒。

---

## 10. 配置（`config.example.yaml` 新增 `audit:` 块）

```yaml
audit:
  enabled: true                       # 是否启用录制（false 时埋点为空操作，零开销）
  dir: "/var/log/webshell/audit"      # 录制根目录
  record_input: true                  # 是否录制输入事件（"i"，默认开启用于取证）；false 仅录 output/resize
                                      # 注：回放侧默认不显示 input，需审计员显式开启（§8.2）
  password_input: mask                # ECHO 关闭时的输入处理：mask（默认，等长掩码）| omit（不记）| plaintext（明文，慎用）
  password_mask_when: echo_off_canonical  # 脱敏判据：echo_off_canonical（默认，精确命中密码提示）| echo_off（严格）
  max_session_bytes: "100M"           # 单会话 cast 上限
  max_total_bytes: "20G"              # 总量配额（最旧优先清理）
  retention_days: 90                  # 保留期（按 ended_at 判定，0 表示不自动清理）
  cleanup_interval: "1h"              # 清扫间隔
  fail_closed: false                  # recorder 打开失败（磁盘/IO 错误）时是否拒绝建会话；默认 false（照常开会话仅告警，fail-open）
  trust_proxy: false                  # 是否信任 X-Forwarded-For
  user_can_view_self: true            # 普通用户是否可回放本人录屏
  admins: ["alice", "auditor"]        # 审计员白名单
```

派生到 `auth.Config`（与 `MaxLongN` 等风格一致，`auth/config.go:261`）：

```go
AuditEnabled   bool
AuditDir       string
AuditRecordIn  bool
AuditPasswordMode string // mask|omit|plaintext
AuditPasswordMaskWhen string // echo_off_canonical|echo_off
AuditMaxBytes  int64
AuditTotalBytes int64
AuditRetention time.Duration
AuditCleanupIvl time.Duration
AuditFailClosed bool
AuditTrustProxy bool
AuditUserSelf   bool
AuditAdmins    map[string]bool
```

---

## 11. 安全与边界

| 风险 | 措施 |
|---|---|
| **密码泄露** | **依据内核 `ECHO` 标志自动脱敏（§3.4）**：`sudo`/`ssh`/`passwd` 等关 echo 的密码提示，输入按 `password_input: mask`（默认）记等长掩码、不落明文，与终端「看不见」行为对齐；raw 模式 TUI 不误伤。逃生口 `omit`（不记）/`plaintext`（需显式开启）。已知盲区：raw 模式 TUI 内的密码输入默认不脱敏（§3.4），可用 `password_mask_when=echo_off` 收紧 |
| **api_key 泄露** | `api_key` 走独立 WS 消息（`auth-request/auth-response`）不经 pty，天然不入 cast（§4）；与 §3.4 互补；header `env` 白名单杜绝敏感变量进 cast header（§3.2） |
| **output 含敏感信息** | 录屏等同终端所见，**output 侧无法脱敏**（`cat 密码文件`/`echo $SECRET` 的输出原样落盘）；属录屏审计固有属性，在 §8.3 用户提示中一并告知 |
| 回放侧输入可见性 | **默认不显示 input**（§8.2）；即便显示，密码块也已是掩码（§3.4），双重保护。仍提供 `record_input=false` 彻底关闭输入录制 |
| 越权查看 | 审计 API 强制 self/auditor 判定；越权 404；文件 0700/0600 |
| 录制防篡改 | 文件属主为服务用户（ease）+ `0700/0600`，会话属主无权删改；hash 链防抵赖列 P5 可选 |
| mgmt_token 泄露 | 沿用现有 XSS 加固（自托管依赖 + CSP，`main.go:57`）；审计员白名单降低高权账号面 |
| 磁盘打满 | 单会话上限 + 总量配额 + 保留期 + 写失败降级丢弃（§4、§6） |
| 录制拖慢终端 | 内存队列 + 异步 flush；满则丢弃计数，绝不阻塞 pty 泵（§4） |
| 隐私合规 | UI 明示录制（含 output 不脱敏的说明，§8.3）；保留期；用户可查本人；审计员操作可再记一条「审计访问日志」（P2 可选） |
| 服务重启 | 会话实体本身跨重启不存活（`DESIGN_SESSIONS.md` §6 P2），故 cast 以「本次进程生命周期」为界。**优雅停机**（`main.go` 新增 signal 处理）先逐会话 `Close("server shutdown")` 再 `Shutdown` 落结束态；**崩溃/强杀**时下次启动由启动巡检补写 `ended_at`/`end_reason`（§6.3），保证无悬空记录；崩溃丢失窗口 ≈ flush 周期（§6.3 披露） |
| 路径穿越 | `sessionID` 由服务端生成（`newSessionID`，`handler/manager.go:394`）；拼接目录时校验只含 `[0-9a-f]`，`username` 来自受信认证 |

---

## 12. 测试矩阵

| 场景 | 预期 |
|---|---|
| 启用后创建会话并执行命令 | `meta.json` + `cast.log` 生成；`o`/`i`/`r` 事件时间戳递增 |
| 关页面（detach）后会话继续跑命令 | detach 期 output 仍写入 cast |
| 重连后 | `attach`/`detach` 时间线正确；回放完整 |
| resize | `r` 事件写入，回放器应用尺寸 |
| 磁盘满 / 写失败 | 终端不受影响；`dropped_events>0`；无 panic |
| `record_input=false` | 无 `i` 事件 |
| 密码提示下输入（`ECHO=0 && ICANON=1`） | cast 中该输入为**等长掩码**（或 `omit` 时缺失），**无明文**；`masked_input_events` 计数正确 |
| raw 模式 TUI 输入（`ICANON=0`） | 不脱敏，正常录入 |
| `password_mask_when=echo_off` | `ECHO=0` 即脱敏（含关 ICANON 情形） |
| `EchoState()` 单测 | termios `c_lflag` 读取正确（echo/canonical）；需 Linux pty 环境（与现有 pty 测试同约束） |
| pty 输出跨块截断多字节字符 | 跨块重组后为单一完整字符，无块边界假 `\ufffd` |
| pty 输出非 UTF-8（如 GBK） | 录制为 `U+FFFD`（有损、无 panic）；cast 每行均为合法 JSON |
| 上传/延期/关闭事件 | `upload`/`extend`/`close-req` 正确入 timeline；`auth-provided` 仅含变量名清单 |
| 清扫与活跃会话 | 活跃（未 Close）会话不被清扫；结束后可被清理 |
| `rec.Close` 后再写入 | 安全丢弃，无 panic、文件不损坏 |
| meta.json 原子写/损坏容错 | temp+rename 原子替换；损坏 meta 按「缺 `ended_at`」补写 |
| cast/download 活跃会话 | 409（不允许回放/下载进行中会话）；meta 仍可读 |
| cast 切片 time 平移 | 窗口内时间从 0 起，`startAt=0` 可播 |
| player 对 `i` 事件（spike） | 记录结论并回填 §8.2；若忽略则自建输入面板生效 |
| CSP 集成：player 加载 | 同源 URL / `{data}` 模式加载成功，无 CSP 拦截 |
| `rec.Open` 失败（fail-open/closed） | 默认会话照常建立并告警；`fail_closed=true` 时拒绝创建 |
| api_key 补全流程 | cast 中**无** api_key 明文 |
| 普通用户查他人录屏 | 404 |
| 审计员查任意录屏 | 200 |
| 非审计员访问 download | 功能拒绝（404/403）；delete 端点已移除（404） |
| 单会话超 `max_session_bytes` | 停止记录并计数，文件不无限增长；`stopped_recording: true` |
| 超保留期 | 清扫任务删除目录（按 `ended_at` 判定） |
| 超总量配额 | 最旧优先删除 |
| 大 cast 分段拉取（t_from/t_to） | 只返回窗口内事件 |
| asciinema-player 回放 | `o`/`r` 正确重放；input 默认不显示，显式开启后可显示（或自建面板生效） |
| 优雅停机（SIGTERM） | 现存会话写 `close(server shutdown)`，`meta.json` 含 `ended_at`/`end_reason` |
| 启动巡检补写残留 cast | 无 `ended_at` 的目录补 `ended_at`+`end_reason=server restart (abnormal)`；已有结束态者跳过（幂等） |
| recorder 单测（内存桩） | 事件顺序、flush、Close 幂等、队列满丢弃 |
| 权限单测 | `IsAuditor`、self/auditor 判定 |
| 配置解析 | 各默认值/派生值；`enabled=false` 时零埋点；`record_input` 缺省为 true |

---

## 13. 文件改动清单

| 文件 | 改动 |
|---|---|
| `pty/session.go` | 新增 `EchoState()`（读 master termios `c_lflag`，供 §3.4 脱敏判据） |
| `audit/recorder.go`（新增） | `Recorder`：asciicast v2 写入、`WriteInputMasked`、**跨块 UTF-8 重组**、内存队列、异步 flush、丢弃计数、`meta.json`（含 `masked_input_events`，**temp+rename 原子写**）、并发契约（Close 后写入丢弃） |
| `audit/store.go`（新增） | 目录布局、列表/读取/切片、清扫（保留期/配额、**排除活跃会话**）、**启动巡检补写残留 cast 结束态（§6.3）** |
| `audit/cast.go`（新增） | NDJSON 事件编解码、header 处理、按时间切片（**含 time 平移**） |
| `audit/*_test.go`（新增） | recorder/store/cast 单测 |
| `handler/manager.go` | `localSession` 加 `rec`；`sendOutput`/`readLoop`/`Attach`/`Detach`/`close` 埋点（`rec.Close` 置步骤⑤后）；`Create` 接受 `ClientInfo` 并 `rec.Open`（失败路径清理、fail-open/closed）；`recFactory` 接缝；活跃会话清单供清扫 |
| `handler/ws.go` | Upgrade 前采集 `ClientInfo`（IP/UA，`handler/ws.go:65`），传入 Create/Attach；`auth-provided` 暂存（rec 创建前）随 meta 落盘 |
| `handler/upload.go` | 上传成功后 `rec.Event("upload", {file, dest})`（§4） |
| `handler/session_api.go` | `extend`/`close-req` 操作补记 timeline（§4） |
| `handler/login.go` | 响应新增 `is_auditor` 字段（§7.2） |
| `handler/audit_api.go`（新增） | 列表/meta/cast/download + `RequireAuditor`；cast/download 对活跃会话 409（无 delete 端点） |
| `auth/config.go` | 新增 `audit:` 配置与派生值；`ExampleConfig` 同步 |
| `config.example.yaml` | 同步 `audit:` 块 |
| `main.go` | 构造 audit store/recorder，注入 manager；注册审计路由；启动清扫 goroutine；**新增优雅停机（signal + 先逐会话 `Close` + `srv.Shutdown`）** |
| `static/vendor/asciinema-player.js` / `asciinema-player.css`（新增） | 自托管 asciinema-player（`//go:embed`，去 `sourceMappingURL`，记录版本 + SHA-256） |
| `static/index.html` / `static/app.js` / `static/css/style.css` | 审计入口、列表页、asciinema-player 回放页（含自建输入面板，如 spike 结论需要）、录制提示 |
| `static_embed.go` | 内嵌新增 vendor 文件 |
| `README.md` / `USER_GUIDE.md` | 文档同步 |

---

## 14. 分阶段实施

| 阶段 | 内容 | 依赖 |
|---|---|---|
| **P0** | `audit` 包：recorder（asciicast v2 写入 + `WriteInputMasked` + 跨块 UTF-8 重组 + 异步落盘 + 丢弃计数）+ 单测 | 无 |
| **P1** | `pty.Session.EchoState`（§3.4）；`handler/manager.go` 埋点接入（output/input[含脱敏判据]/resize + open/attach/detach/close[步骤⑤后] + upload/extend 事件）；`ws.go` 采集 `ClientInfo`；`Create` 接缝（fail-open/closed + 失败路径清理） | P0 |
| **P2** | `audit/store` + 清扫（保留期/配额/单会话上限/排除活跃）；**main.go 优雅停机 + 启动巡检补写残留 cast 结束态（§6.3）**；审计 API + `RequireAuditor`；配置项 | P1 |
| **P3** | **spike：验证 asciinema-player 对 `i` 事件支持（结论回填 §8.2）**；前端：审计入口、列表、**自托管 asciinema-player 回放页**（含自建输入面板，如需）、录制提示 | P2 |
| **P4** | 权限与合规收尾：`user_can_view_self`、下载取证、审计访问日志（可选）、文档与验收测试 | P2,P3 |
| **P5（可选）** | 大文件关键帧/分段优化 + `startAt`；AD 组作为审计员来源；hash 链防抵赖；base64 扩展字段（字节级保真，偏离标准） | P4 |
| **下一版本** | **实时直播**：审计员实时观看在线会话（见 §15） | P4 |

---

## 15. 已确认决策

以下决策已评审确认，作为实施依据：

1. **回放格式**：采用 **asciicast v2 录制 + 引入 asciinema-player** 回放。player 必须自托管（`//go:embed`，去 CDN，记录版本 + SHA-256），脚本面 CSP 保持 `script-src 'self'` 不放宽；cast 加载仅用**同源 URL 或 `{data}` 内联**，**禁用 Blob URL**（`connect-src 'self'` 不含 `blob:`）；player 对 `i` 事件的支持以 §8.2 spike 结论为准（若忽略则自建输入面板）。
2. **输入录制默认值**：`record_input` **默认开启**（完整取证）；回放侧**默认不显示 input**，由审计员显式开启查看（§8.2、§11）。
3. **审计员来源**：起步使用**配置用户名白名单** `audit.admins`（最小改动，§7.1）；AD 组作为后续可选来源。
4. **持久化与清理**：会话不跨重启存活；**`main.go` 新增优雅停机**（signal + 先逐会话 `Close("server shutdown")` + `srv.Shutdown` 兜底）；崩溃/强杀后的残留 cast 由**启动巡检补写结束态**（§6.3）。保留期/配额清扫照常（排除活跃会话）。
5. **实时直播**：**下一版本**再实现（审计员实时观看在线会话，需在 `Attach` 时广播一份只读副本）。本版本不做，列为 P5 之后独立迭代。
6. **密码脱敏**：采用**内核 `ECHO` 标志**作为判据（`ECHO=0 && ICANON=1` 精确命中密码提示，raw 模式 TUI 不误伤），`password_input` **默认 `mask`**（等长掩码、不落明文），对齐终端「看不见」行为；提供 `omit`/`plaintext` 逃生口（§3.4）。
7. **rec.Open 失败语义**：默认 **fail-open**（照常开会话 + 告警 + 计数）；提供 `audit.fail_closed` 开关供合规收紧（§4、§10）。
8. **rec.Close 时序**：置于会话 close 六步的**步骤⑤（proc.Close）之后**；recorder 并发安全、Close 后写入丢弃、meta 原子写（§4、§4.1、§6.3）。
9. **output 不脱敏**：录屏等同终端所见，output 侧不做脱敏，在用户提示中明示（§8.3、§11）。
10. **审计数据只读**：**不提供任何人工删除能力**（移除 `POST /api/audit/delete` 端点、`Store.Delete`、前端「删除」按钮），清理仅由保留期/总量配额自动执行（§6.2）；**active（进行中）会话不可回放/下载**——`/api/audit/cast`、`/api/audit/download` 对 active 返回 `409 {"error":"active session"}`，前端亦不渲染「回放」「下载」按钮；`/api/audit/meta` 保持对 active 可读以展示状态（§7.2、§9）。
