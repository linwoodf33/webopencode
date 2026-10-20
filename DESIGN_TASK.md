# 后台任务系统设计方案（DESIGN_TASK）

> 状态：方案设计（待评审）
> 关联需求：用户在 Web 终端运行长任务，关闭页面/会话超时后进程被 Kill，任务丢失，需支持"关闭页面任务继续在后台跑，随时回来查看"。

---

## 1. 目标与问题定义

### 1.1 要解决的问题

当前交互式 pty 会话的生命周期（`handler/session_hub.go`）：

| 场景 | 行为 | 代码位置 |
|------|------|---------|
| 关闭页面 | 仅 `detach`（`conn=nil` + 重置 5 分钟 TTL），进程不杀 | `session_hub.go:110` |
| 5 分钟内刷新/重连 | 同 sessionID 恢复，任务不中断 | `ws.go:34-44`、`session_hub.go:73` |
| 超过 5 分钟不回来 | `reaper` 巡检触发 `expire` → `sess.Close()` → Kill 整个进程树 | `session_hub.go:287`、`pty/session.go:621` |
| 任务自己跑完退出 | `pump` 读到 EOF → `closeSession` → Kill 进程树 | `session_hub.go:158` |

结论：长任务最多保活 `sessionTTL = 5 * time.Minute`，超时后进程树（sudo→sandbox-init→bash→opencode/codex）被强制 Kill，任务丢失，只能重开。

### 1.2 设计目标

1. 长任务**脱离 WebSocket 连接**，关闭页面后继续在后台运行；
2. 用户随时重新登录，在**任务列表**中查看运行状态、实时/历史日志；
3. 可**终止/删除**任务；
4. 任务仍运行在现有沙箱隔离内（cgroup/seccomp/rootfs/降权），不破坏安全模型。

### 1.3 范围界定

任务系统面向**非交互式命令**（`python script.py`、`make build`、`./train.sh` 等）。
opencode/codex 是交互式 TUI，不在本方案范围内（进阶可支持 headless 模式，见 §9）。

---

## 2. 总体架构

```
+------------------- 浏览器 -------------------+
|  终端(xterm)       任务面板(Task UI)          |
|  [提交任务]        [列表/日志/终止]            |
+----------------------+------------------------+
                       | HTTP/WS
+----------------------v------------------------+
|                WebShell 服务(Go)               |
|   handler/session_hub.go  (现有交互会话)       |
|   handler/task*.go        (新增任务子系统)     |
|        |                      |               |
|   pty.Session(交互)      task.TaskManager     |
|        |                 + task.executor      |
|        |                 + task.logstore      |
+----------------------+------------------------+
                       |  spawn(沙箱后台模式)
+----------------------v------------------------+
|    sandbox-init --program=task --log-file=...  |
|    (复用 cgroup/seccomp/rootfs/降权)           |
|    setsid + stdout/stderr -> 日志文件          |
+-----------------------------------------------+
```

### 核心决策 C1：任务进程模型

任务作为**独立进程组**由 `sandbox-init` 以新增的后台模式启动：
- **不挂 pty、不依赖任何 WebSocket 连接**；
- 输出全部重定向到日志文件；
- 进程 `setsid` 脱离会话。

这是与现有交互会话在进程模型上的根本区别。

---

## 3. 数据模型

新增 `task` 包（`task/`），核心结构：

```go
// TaskStatus 任务状态
type TaskStatus string
const (
    TaskPending    TaskStatus = "pending"     // 排队中（尚未 spawn）
    TaskRunning    TaskStatus = "running"     // 运行中
    TaskSucceeded  TaskStatus = "succeeded"   // 正常退出 code 0
    TaskFailed     TaskStatus = "failed"      // 非零退出
    TaskKilled     TaskStatus = "killed"      // 被用户/系统终止
    TaskLost       TaskStatus = "lost"        // 进程消失但退出码未知（异常）
)

type Task struct {
    ID         string     `json:"id"`         // 32 hex，同 newSessionID 机制
    Username   string     `json:"username"`   // 属主（权限边界）
    Mode       string     `json:"mode"`       // bash | opencode-headless
    Command    string     `json:"command"`    // 用户输入的命令（非交互，单命令）
    CWD        string     `json:"cwd"`        // 工作目录（默认 homeDir）
    Status     TaskStatus `json:"status"`
    ExitCode   *int       `json:"exitCode,omitempty"`
    PID        int        `json:"pid,omitempty"`     // sandbox 首进程 pid
    CreatedAt  time.Time  `json:"createdAt"`
    StartedAt  time.Time  `json:"startedAt,omitempty"`
    FinishedAt time.Time  `json:"finishedAt,omitempty"`
    LogPath    string     `json:"logPath"`     // 日志文件绝对路径
    LogOffset  int64      `json:"logOffset"`   // 已推送/已读到的日志游标
}
```

### 核心决策 C2：存储

- 任务元数据：**TaskManager 内存 map + 可选 SQLite 持久化**（见 §10）；
- 日志：落**文件**（追加写，支持 seek 分页/tail）。

---

## 4. 生命周期状态机

```
  [提交] → pending → running ─┬─ 退出码 0    → succeeded
                              ├─ 退出码 !=0  → failed
                              ├─ 被 kill      → killed
                              └─ 进程异常消失 → lost
  任一路径后：TaskManager.remove(超期保留策略) → 可删除
```

- `pending`→`running`：调度器判定可运行（§5.6）后 executor 成功 spawn，置 running；
- `pending` 停留期间即"排队中"（未轮到/等待资源），可被用户取消（DELETE）出队；
- 结束判定：`cmd.Wait()` 返回（前台 wait goroutine）或 `ProcessState` 就绪；
- `killed`：`POST /api/task/kill` 或系统 cgroup 强制终止。

---

## 5. 后端设计

### 5.1 TaskManager

```go
type TaskManager struct {
    mu     sync.Mutex
    tasks  map[string]*Task        // id -> task
    byUser map[string][]string     // username -> taskIDs（排序/分页用）
    logDir string                  // 日志根目录
    events *eventHub               // 任务事件广播（WS/SSE）

    // ---- 调度相关 ----
    queues      []string          // pending 队列（FIFO，按提交时间）
    running     map[*Task]bool    // 当前 running 集合
    perUserRun  map[string]int    // 用户 -> 该用户 running 数
    maxPerUser  int               // 单用户最大并发
    maxGlobal   int               // 全局最大并发（0 表示不限制）
    wakeCh      chan struct{}     // 调度唤醒信号（任务结束/取消排队时触发）
}
```

职责：
- `Submit(username, mode, command, cwd, env) (*Task, error)`：校验 → 建 task（pending）→ **判定运行或入队（见 §5.6）** → 返回 taskID；
- `Get(id) / List(username, offset, limit) / Kill(id) / Delete(id)`；
- `onFinished(task)`：任务结束时释放并发额度并唤醒调度器（重新评估 pending 队列）；
- 通过 **eventHub** 广播状态变更与日志增量（供 `/ws/tasks` 推送）。

### 5.2 executor（任务执行器）

**核心：如何让任务脱离连接、以目标用户身份、在沙箱内后台运行。**

新增 `sandbox-init` 后台模式（复用现有 `pty.NewSessionForUserMode` 的启动链路，但输出不走 pty）：

```
/usr/bin/sudo -n -u root <InitPath> \
    --uid <uid> --gid <gid> --home <homeDir> \
    --session <taskID> --program task \
    --command '<cmd>' --cwd <cwd> --log-file <logPath> \
    --rootfs <rootfs> --network <net> --memory <mem> \
    --cpu <cpu> --pids <pids> ... (其余沙箱参数同现有)
```

在 `sandbox/sandbox.go` 中新增分支（`execProgram` 扩展，或新增 `execTask`）：
1. 沿用 `Run`→`enterCgroup`→`execUsernsInit`→`childInit` 全链路（cgroup/seccomp/pivot_root/降权全部保留）；
   - uid/gid/home 由 executor 从 `user.Lookup(属主用户名)` 派生（复用 `pty/session.go` 现有逻辑）；
   - `childInit` 末尾 `dropToUser`：setgroups（保留补充组）→ setgid → setuid **降权到属主真实用户（非 root）**（`sandbox/sandbox.go:306`）；NFS 等组权限访问与宿主机一致，沙箱内能力 = 属主自身权限；
2. `childInit` 内对 `program==task`：**不开 pty、不设 PS1/LANG 交互环境**；
3. `setsid()`（脱离控制终端与会话，避免 SIGHUP 传染）；
4. `stdout/stderr → os.OpenFile(logPath, O_APPEND|O_CREATE|O_WRONLY, 0600)`；
5. `exec /bin/bash -c '<cmd>'`（或 `env -i` + 目标用户环境）。

> **非沙箱模式**（`sandbox.enabled=false`，现有传统 sudo 路径）fallback：
> `sudo -n -u <user> setsid bash -c 'cd <cwd> && <cmd>' > <log> 2>&1 &`
> 同样脱离连接，但无 cgroup/seccomp 隔离（与现有交互模式行为一致）。

### 5.3 日志存储 logstore

- 日志文件：`<logDir>/<username>/<taskID>.log`，权限 `0600` 属主为用户；
- **写入端**：executor 直接追加重定向；
- **读取端**：
  - `GET /api/task/log?id=<id>&username=&offset=&limit=`：`os.Open` + `ReadAt`/`Seek`，支持任意 offset 分页；
  - **增量推送**：executor/manager 的 wait goroutine 轮询文件新增字节（或 exec 时用 pipe tee 进文件 + 内存环形缓冲），通过 eventHub 把新行推给订阅的 WS。推荐**文件 Seek(tail) 轮询**（间隔 ~500ms），简单可靠、天然支持"断线重连后回放"。

### 5.4 API 设计（新增路由）

| 方法/路径 | 说明 |
|---|---|
| `POST /api/task` | body `{username, mode, command, cwd}` → `{id}`。username 与登录态校验（复用 AD token 机制，需校验请求者==属主） |
| `GET /api/tasks?username=&offset=&limit=` | 任务列表（按创建时间倒序） |
| `GET /api/task?id=<id>&username=` | 任务详情 |
| `GET /api/task/log?id=<id>&username=&offset=&limit=` | 日志分页读取 |
| `POST /api/task/kill?id=<id>&username=` | 终止任务 |
| `DELETE /api/task?id=<id>&username=` | 删除历史任务（含日志文件） |
| `WS /ws/tasks?username=` | 实时推送：`{type:"task-status", data:{id,status,exitCode}}` / `{type:"task-log", data:{id,lines:[...]}}` |

**鉴权**：所有任务 API 复用现有登录后的一次性 token 校验流程（`authr.ValidateAndConsume`），并强制 `request.username == 属主`，防止越权查看他人任务/日志。

### 5.5 事件推送

新增 `eventHub`（订阅/发布模式）：任务状态变更、日志增量通过 hub 广播；`/ws/tasks` 的每连接按 `username` 订阅。复用现有 `protocol.Message` 信封（`protocol/message.go`），新增消息类型 `task-status` / `task-log`。

### 5.6 提交判定与排队调度

**核心原则**：提交的任务先尝试"直接运行"；不满足并发条件则入 FIFO 队列等待，由调度器在资源释放时唤醒。`pending` 状态即代表"排队中"。

**直接运行的判定（全部满足才运行，否则入队）**：

1. **单用户并发**：`perUserRun[username] < maxPerUser`；
2. **全局并发**：`maxGlobal == 0` 或 `len(running) < maxGlobal`；
3. **资源预算（可选进阶）**：任务声明内存/CPU 需求时（如 `--memory 2G`），检查全局资源池剩余是否足够；不足则排队。

**调度算法**（FIFO + 触发式）：

```text
Submit(t):
    t.Status = pending; 入队 queues
    if 满足运行条件(t):
        出队 -> running -> spawn
    else:
        等待唤醒

onFinished(t) 或其他资源释放:
    释放并发额度
    循环尝试取出队头任务，满足条件则弹出并 spawn，直到队头不满足
    （保证先到先得，防止饥饿）
```

- **唤醒机制**：任务结束（succeeded/failed/killed/lost）或取消排队时，向 `wakeCh` 发信号，调度器 re-select 队头；
- **公平性**：单用户内严格 FIFO（按提交时间）；多用户间可选用 round-robin（进阶），起步用全局 FIFO 即可；
- **排队超时**：可配置 `queue_timeout`（如 1h），超时仍未轮到则任务标记为 `failed`（原因 "queue timeout"）并出队，避免无限占位；
- **不在队列中的操作**：`pending` 任务可被用户 `DELETE` 取消（直接从队列移除，不 spawn）；`kill` 仅对 `running` 生效（对 `pending` 等价于取消入队）。

**为什么需要队列**：每个任务独立 cgroup，若多任务同时跑会叠加占据主机 memory/CPU（如 `--memory 1G` × 20 并行 = 20G），可能 OOM 或严重影响交互会话；pids 上限同理。并发上限是保护主机与同机其他用户的必要手段。

---

## 6. 前端设计（static/app.js + index.html）

### 6.1 新增"任务"视图

- 登录/模式选择后，工具栏增加 **任务(Tasks)** 入口，进入任务列表页；
- 任务列表：每行显示 `状态徽章(running绿/succeeded蓝/failed红/killed灰)`、命令摘要、创建时间、退出码；
- 运行中的任务徽章可实时闪烁/更新（经 `/ws/tasks` 或 5s 轮询）。

### 6.2 任务详情（日志查看）

- 打开任务 → xterm 只读实例或 `<pre>` 渲染日志，**自动跟随尾部**（类似 `tail -f`）；
- 支持"回到顶部/跳到底部/暂停跟随/复制"；
- 日志通过 `GET /api/task/log` 初次回放（从 offset 0），之后走 `/ws/tasks` 增量或轮询增量。

### 6.3 提交任务入口（两种）

1. **终端快捷方式**：在交互终端输入 `task <command>`（前端拦截该前缀，调用 `POST /api/task`，不在终端直接执行）；
2. **任务面板表单**：任务列表页内嵌"提交新任务"表单（mode 下拉 bash/opencode-headless + 命令输入）。

> 交互终端与任务系统**完全解耦**：交互终端仍是原来的 pty 会话；任务不会出现在交互终端里，只在任务面板查看。

---

## 7. 安全与资源控制

| 风险 | 措施 |
|---|---|
| 越权访问 | 所有 API 校验属主 username；日志文件 0600 |
| 越权/提权 | 任务以**属主真实 uid/gid 降权运行**（setgroups 保留补充组，非 root），与交互会话一致；沙箱内能力 = 属主自身权限 |
| 命令注入 | 命令由属主自己输入，风险等价于交互 shell，无新增面；`--command` 经 argv 传递不经 shell 二次解释（`bash -c` 由沙箱内部构造） |
| api_key 泄露进日志 | 提交时对命令做**脱敏**（匹配 `KEY=...`/`-k <key>` 模式替换为 `***`）；日志文件权限收窄 |
| 资源失控 | 复用 cgroup memory/cpu/pids 限制（任务也进 cgroup），与交互会话一致 |
| 日志膨胀 | logstore 设单文件上限（如 100MB），超限轮转/截断；任务删除即清日志 |
| 孤儿进程 | setsid + wait goroutine + cgroup 兜底清理（复用现有 userns-init cleaner 机制） |

---

## 8. 进程清理与孤儿回收

- **正常结束**：executor 的 wait goroutine 收到 `ProcessState` → 更新状态 → 通知 eventHub → 复用现有 cgroup 会话清理（userns-init cleaner 移 pid 到根 cgroup + rmdir，见 `sandbox/userns_init.c:66`）；
- **服务重启**：重启后内存 TaskManager 丢失 → 恢复策略：扫描 logDir 中残留日志 + 对应 cgroup 目录（`/sys/fs/cgroup/webshell_sandbox/<taskID>`）判断孤儿任务，标记为 `lost`（cgroup 有进程在跑）或 `succeeded/failed`（已退出）；
- **手动 kill**：`POST /api/task/kill` → 复用现有 Session.Close 杀进程树的链路（`sess.Close()` → Kill 整个进程组），再走 cgroup 清理。

---

## 9. 可选进阶（opencode/codex headless）

- opencode 支持非交互执行：`opencode run "<prompt>"`（headless）。可让"提交任务"时选 opencode 模式 → 任务命令实际为 `cd <homeDir> && <opencodePath> run --session-prompt '<prompt>'`，在沙箱后台跑，api_key 复用现有 `.auth.env` 注入链路；
- codex 同样有 headless `codex exec`。**此阶段先不做**，作为后续迭代，因为依赖具体 CLI 版本行为。

---

## 10. 持久化（C2 细化）

- **起步（推荐）**：任务元数据仅内存 + 日志文件。服务重启后靠 §8 恢复策略重建任务列表（状态可恢复、历史日志保留）。**改动最小**，符合当前项目"轻量、无外部依赖"的风格；
- **进阶（可选）**：引入 SQLite（`modernc.org/sqlite` 纯 Go，无 CGO）持久化任务元数据，重启不丢状态字段、支持按用户/状态/时间精确查询与分页。

---

## 11. 配置项（config.example.yaml 新增 `tasks:` 块）

```yaml
tasks:
  enabled: true            # 是否启用后台任务系统
  log_dir: "/var/log/webshell/tasks"   # 日志根目录（服务用户可写或 root 预建）
  max_log_size: "100M"     # 单任务日志上限
  max_tasks_per_user: 20   # 每用户保留任务数（超限删最旧）
  max_concurrent_per_user: 2   # 单用户最大并发任务数，超出入 FIFO 队列
  max_concurrent_global: 8     # 全局最大并发（0 表示不限制）
  queue_timeout: "1h"      # pending 排队超时，超时标记 failed（0 不超时）
  default_cwd: ""          # 空则默认用户 homeDir
```

---

## 12. 测试与验收

### 单元测试

- TaskManager 状态机（pending→running→succeeded/failed/killed/lost 全路径）；
- **调度判定**：并发上限内直接运行、超限入队、任务结束后队头出队（先到先得）、队列超时取消、pending 取消出队不 spawn；
- logstore 分页/offset/tail 正确性；日志脱敏；
- 属主鉴权（他人 username 一律 403/404）；
- API handler 参数校验。

### 集成测试（目标机 RHEL 9.5 验证）

1. `POST /api/task` 提交 `sleep 120; echo done` → 关闭页面 → 等待 >5min（超过 sessionTTL）→ 重新登录 → 任务仍在 running → 日志可见；
2. `sleep 120` 结束后 → succeeded，退出码 0，日志含 "done"；
3. `kill` 一个 running 任务 → killed，进程树/cgroup 清理干净（`ps`、`/sys/fs/cgroup/webshell_sandbox/<id>` 消失）；
4. 非沙箱模式 + 沙箱模式各跑一遍；`id` 显示目标用户（降权正确、补充组保留）；
5. 越权：用户 A 访问用户 B 任务 → 拒绝；
6. 服务重启后孤儿任务恢复为 `lost`。

---

## 13. 分阶段实施计划

| 阶段 | 内容 | 依赖 |
|---|---|---|
| **P0** | `task` 包：Task 模型 + TaskManager + eventHub + logstore（内存版） | 无 |
| **P1** | `sandbox-init` 新增 `--program=task` 后台模式（setsid + 日志重定向 + 复用 cgroup/降权）；非沙箱 fallback | P0 |
| **P2** | API + 路由 + 鉴权 + 进程清理/孤儿恢复 | P0,P1 |
| **P3** | 前端任务面板 + 日志实时推送（/ws/tasks）+ 提交入口 | P2 |
| **P4** | 配置项、日志脱敏、上限策略、验收测试 | P2,P3 |
| **P5（可选）** | SQLite 持久化；opencode headless 任务 | P4 |

---

## 14. 待确认决策点

1. **持久化**：起步用"内存+日志文件+重启恢复"（推荐，改动小），还是直接上 SQLite？
2. **提交方式**：终端里 `task <command>` 快捷命令 + 任务面板表单都做，还是先只做任务面板表单？
