# 登录白名单设计方案（DESIGN_WHITELIST）

> 状态：已实施并合入（2026-10）。本文档与 `auth/whitelist.go` 实现对照。
> 关联需求：登录闸门——AD 认证通过且系统存在该用户后，还须命中白名单才签发 `mgmt_token`。
> 关联模块：`auth/*`、`handler/login.go`、`main.go`。
> 关联文档：`DESIGN_SESSIONS.md`（认证/凭证）、`README.md`「登录白名单」、`DEPLOY.md`（配置与迁移）。

## 1. 背景与目标

- **现状演进**：早期方案按 `/etc/group` 中某个组（`users_group`，默认 `webusers`）的组员名单放行。该方案把"谁能登录"绑定在操作系统用户组状态上，与部署拓扑耦合、迁移成本高。
- **目标**：
  1. 白名单来源改为**独立配置文件**，与操作系统解耦；
  2. **热更新**：编辑名单文件后，用户下一次登录立即按新名单放行/拒绝，**无需重启服务**；
  3. 保持 `auth` 包对外 API 最小变动，**零新增第三方依赖**、无后台调度依赖。

## 2. 配置与三态语义（fail-closed 默认）

| 配置 | 说明 |
|---|---|
| `web_users_file` | 白名单文件绝对路径；**空（缺省）= 未配置** |
| `web_users_fail_open` | 逃生阀；`true` 时无条件全放行；缺省 `false` |

三态行为：

| 场景 | 行为 | 日志 |
|---|---|---|
| `web_users_fail_open: true` | 恒放行（逃生阀） | 启动 warn 一次 |
| 未配置（`web_users_file` 为空）+ 非 fail-open | 恒拒绝（默认 fail-closed） | 无 |
| 已配置但文件缺失 / 为空 / 全注释 | 全拒（fail-closed） | error / warn |

**决策依据**：
- 安全功能**默认拒绝**：无名单即无准入，行为可预期、可审计。
- 与旧方案的迁移**零行为回退**：旧方案"`webusers` 组不存在 → 全拒"，新方案"未配文件 → 全拒"一致；运维须显式落一份名单。
- 放行意图用**显式逃生阀**表达，而非依赖"删文件/清空"这类隐式动作（避免编辑器 truncate-then-write 造成瞬时误开放）。

## 3. 白名单文件格式

- 纯文本，一行一个用户名（服务建议部署于 `/etc/webshell/web_users`，权限 0600）。
- 整行以 `#` 开头为注释；空行忽略；每行 `TrimSpace`；**不支持行尾 `#` 注释**（会被视为非法行跳过并告警）。
- 用户名须匹配 `^[a-z_][a-z0-9_-]{0,31}$`，非法行跳过并在该次加载记聚合 warn。
- 匹配**大小写不敏感**：加载时将条目 `ToLower` 后存入 `map[string]struct{}`（天然去重、O(1) 查询）；查询时同样 `ToLower` 后**精确匹配**，杜绝子串误判。

```text
# /etc/webshell/web_users
alice
bob
```

## 4. 热更新机制（mtime 惰性轮询）

**选型**：优先零依赖、零 goroutine——采用 **mtime 惰性检测**，而非 fsnotify（避免引入 `github.com/fsnotify/fsnotify` 改 go.mod/go.sum）或后台 ticker（延迟不满足"下一次登录即生效"）。

- 每次 `Allowed()` 先 `os.Stat(path)`，将 `(ModTime, Size)` 与快照比对：
  - 相同 → 命中缓存，直接查 `map`；
  - 变化（或首次/上次加载失败） → 串行 reload：重读文件 → `parseWhitelist` → 构建新快照 → `atomic.Pointer.Store` 原子替换。
- **并发安全**：`atomic.Pointer[wlSnapshot]` 不可变快照 **无锁读**；`sync.Mutex` 仅串行化 reload 段；Login 与 reload 并发时读到的一定是"旧完整快照"或"新完整快照"（线性化一致），绝不半新半旧。
- **fail-stale（读失败降级）**：reload 读取失败（权限/损坏/被编辑半程）→ **保留上次成功快照继续裁决**（既不全拒也不 fail-open），仅记 error；首次加载失败则 fail-closed（空名单全拒）。防止"编辑半程瞬间锁死所有人或放开所有人"。
- **日志抑制**：快照携带 `loadErr`，同错不重复刷屏；白名单拒绝本身不打日志（`handler/login.go` 已记 login 失败与原因）。

**边缘**：极端"同秒同字节数改内容"可能漏检一次，下下次 login 的 stat 因 mtime 已推进必检出，属可接受弱一致。

## 5. 结构设计

```go
// auth/whitelist.go
type wlSnapshot struct {
    users   map[string]struct{} // 小写归一；nil 表示 fail-closed 空集语义
    modTime time.Time
    size    int64
    n       int                 // 有效用户数（日志）
    loadErr string              // 最近读取错误串（同错日志抑制）
}
type UserWhitelist struct {
    path     string
    failOpen bool
    mu       sync.Mutex         // 仅串行化 reload 段
    state    atomic.Pointer[wlSnapshot]
}
func newUserWhitelist(path string, failOpen bool) *UserWhitelist
func (w *UserWhitelist) Allowed(username string) bool // 惰性 stat+mtime 检测 → 快照查询
func (w *UserWhitelist) LoadOnce() error              // 强制重载（main 预热/诊断）
func (w *UserWhitelist) Start(ctx context.Context)    // 惰性方案：仅预热一次；为未来引擎演进预留
func (w *UserWhitelist) Stop()                        // 幂等；惰性方案无后台活动
```

- `NewAuthenticator(cfg)` 签名不变；`Authenticator` 持 `whitelist *UserWhitelist`，`Login` 判定委托 `Allowed()`。
- **判定位置**：保持原第 7.5 步（系统存在校验 `user.Lookup` 之后、签发 `mgmt_token` 之前），不提前到 AD 认证前——保持既有错误优先级，最小化行为变化。
- `main.go` 在 `NewAuthenticator` 后做一次**预热 `LoadOnce()`**（不 fail-fast，失败仅告警 + fail-closed 兜底），启动即暴露配置错误、启动即锁定名单。

## 6. 数据流

```
config.yaml (web_users_file) ──> UserWhitelist(path, failOpen)
                                      │
<名单文件> ──parseWhitelist(ToLower/去重/非法行跳过)──> 快照 (atomic.Pointer)
                                      │ mtime+size 惰性比对
Authenticator.Login ──Allowed(username)──> 命中→继续签发 mgmt_token；未命中→ErrNotEnabled
                                              │
                                     handler/login.go → 403「账号未开通,请联系管理员开通后使用」
```

## 7. 测试与验收

- 单测（`auth/whitelist_test.go`，纯 Go，Windows/macOS 可跑）：解析（注释/空行/CRLF/去重/非法行）、大小写、精确匹配（防 `alice1`/`alice_`/`alice-` 误判）、三态、热更新（改文件 + `os.Chtimes` 推 mtime 后下一次 Allowed 生效）、fail-stale、并发（`go test -race`）。
- 认证集成（`auth/auth_test.go`）：`NewAuthenticator` 接线，未配置=fail-closed、fail-open 语义。
- 配置（`auth/config_test.go`）：新键解析与缺省、旧 `users_group` 键忽略。
- **验收标准**（Linux 端到端）：
  1. 名单含 alice：alice 登录 200，bob 登录 403「账号未开通」；
  2. **不重启**向文件追加 bob → 下一次 bob 登录 200，alice 仍 200；
  3. 删除 alice → 下一次 alice 403；清空文件 → 全员 403 + warn 一次；
  4. 删除文件 → 全员 403、进程不崩、error 仅一次（不刷屏）；
  5. 名单被改坏 → 按旧快照裁决（fail-stale），无瞬时全开/全锁；
  6. `web_users_fail_open: true` → 全员 200（fail-open），启动 warn。

## 8. 运维与迁移（详见 DEPLOY.md）

- 升级（从旧 `users_group`）：`getent group webusers | cut -d: -f4 | tr ',' '\n' | sed '/^$/d' > /etc/webshell/web_users`，配置 `web_users_file`，**删除旧键 `users_group`**（已失效、出现会被忽略）；过渡期可临时 `web_users_fail_open: true`，确认名单后再改回。
- 白名单文件建议 0600、属主服务用户；增删名单即时生效，无需重启。
- 默认 fail-closed：升级后未配置 `web_users_file` 时所有登录被拒，属预期安全默认。
