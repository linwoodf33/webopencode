# PBS 作业集成设计方案（DESIGN_JOBS）

> 状态：方案设计（v1.0，待评审后实施）
> 关联需求：为 Web Shell 用户提供**非交互批处理作业**的提交 / 列表 / 详情 / 日志 / 取消能力；复用集群现有的 **Altair PBS Professional** 调度系统，不自行实现执行器。
> 关联模块：新增 `pbs/*`、`handler/jobs_*.go`、`auth/*`（复用 mgmt_token）、`static/*`
> 关联文档：`DESIGN_SESSIONS.md`（交互式多会话）——本文与其**职责分离**。

---

## 1. 目标与定位

### 1.1 与交互式会话的边界

| 维度 | 交互式会话（DESIGN_SESSIONS） | 批处理作业（本文） |
|---|---|---|
| 本质 | 有人交互的终端（bash/opencode/codex） | 非交互命令/脚本 |
| 执行位置 | **登录节点**（webshell/sessiond 持有 pty，沙箱隔离） | **PBS 计算节点**（调度器分配） |
| 生命周期 | webshell/sessiond 持有 | **PBS 持有**（qsub/qstat/qdel） |
| 隔离/配额 | webshell 沙箱（cgroup/seccomp/pivot_root） | **PBS 队列/资源/账号配额** |
| 服务重启 | 交互会话需 sessiond 守护（DESIGN_SESSIONS §6） | **不受影响**（PBS 持有） |
| 查询入口 | 会话列表 | 作业列表（qstat） |

**原则**：不自行实现任务执行器、日志收集、调度；**一切交给 PBS**。webshell 只做「以登录用户身份调用 PBS 客户端命令 + 解析 + UI」。

### 1.2 设计目标

1. 提交作业（可选队列、CPU/内存/时长、工作目录、输出文件）。
2. 列出当前用户的作业（状态、队列、资源、耗时）。
3. 查看作业详情与**实时日志**（stdout/stderr）。
4. 取消作业；可选 hold/release/rerun。
5. 全流程以**登录 AD 用户身份**执行，作业归属/配额/记账与命令行提交完全一致。
6. 安全：无命令注入、严格字段白名单、作业归属校验、提交限流。

### 1.3 范围界定

- 面向**批处理**（`qsub` 非交互）。`qsub -I` 交互式作业因涉及跨节点 pty 转发，列为**后续扩展**（§14）。
- 不支持作业数组 / 依赖 / 复杂 -W 选项（P2 可选）。
- 作业在计算节点执行，**登录节点沙箱不作用于作业本体**（隔离由 PBS 负责）。

---

## 2. 环境现状（实测）

| 项 | 实测值 |
|---|---|
| 调度器 | **Altair PBS Professional 18.1.4**（`PBS_EXEC=/opt/pbs`，`PBS_HOME=/var/spool/pbs`） |
| 客户端命令 | `/opt/pbs/bin/`：`qsub qstat qdel qhold qrls qrerun qalter qselect qmgr qmove qmsg ...`（**不在 PATH**） |
| 服务名 | `easemgt` |
| 队列 | `workq`（Execution）；`max_queued=[o:PBS_ALL=2000]`，`resources_max.ncpus=32` |
| 负载 | 总量约 3500，排队/运行数百（繁忙集群） |
| JSON 输出 | **不支持** `qstat -F json`（18.1 无 JSON）→ **必须文本解析** |
| `qsub -I` | 支持（交互式，后续扩展用） |
| 客户端可用性 | 仅登录节点 `hpc-node-u01 (10.195.86.188)` 装了客户端；`hpc-node-h02 (10.195.86.126)` **未装** |

> **前提**：webshell 所在主机必须安装 PBS 客户端并配置 `/etc/pbs.conf`（当前登录节点满足）。若部署到其他节点需先装客户端。

---

## 3. 总体架构

```
浏览器 ──HTTP(JSON, Bearer mgmt)──► webshell(job API)
                                        │  exec.Command + sudo -n -u <user> --
                                        ▼
                              /opt/pbs/bin/{qsub,qstat,qdel,...}
                                        │
                                        ▼
                              PBS Server(easemgt) ──► 计算节点执行作业
```

- webshell **不持有**作业进程；仅作为 PBS 客户端的「代理 + 解析 + UI」。
- 每次操作用 `sudo -n -u <user> -- /opt/pbs/bin/<cmd> ...`，使 PBS 的属主/配额/记账归该用户。
- 作业持久性由 PBS 天然保证（webshell 重启无影响）。

---

## 4. 身份与凭证

- 沿用 `DESIGN_SESSIONS.md` 的 **`mgmt_token`**（登录后签发、`Bearer`、绑定 username、TTL `mgmt_ttl_hours`）。作业接口全部要求 `Bearer mgmt`。
- 可选：给 mgmt_token 增加**作用域**（`scopes: [session, job]`），以便账号可按需只授予其一；P1 可先共用不分域。
- 解出 `username` 后：所有 PBS 命令以 `sudo -u <username>` 执行；**作业归属校验**：操作某个 job 前，其 `Job_Owner` 必须等于该 username。

---

## 5. 作业模型

### 5.1 提交参数（前端表单 → API）

| 字段 | 说明 | 校验 |
|---|---|---|
| `name` | 作业名 `-N` | `[A-Za-z0-9_.-]{1,64}` |
| `script` | 作业脚本（`#!` 可选），经 **stdin** 传给 qsub | 长度上限（如 256KB） |
| `queue` | 队列 `-q` | 必须在 `allowed_queues` |
| `ncpus` | 每节点 CPU 数 `-l select=1:ncpus=N` | 整数，`1..max_ncpus` |
| `mem_gb` | 内存 `:mem=NG` | 整数，`1..max_mem_gb` |
| `walltime_hours` | 墙钟时长 `-l walltime=HH:MM:SS` | `>0` 且 `≤ max_walltime_hours` |
| `workdir` | 工作目录 `-d` | 必须位于 `workdir_allow_under` 之下 |
| `stdout` / `stderr` | 输出路径 `-o` / `-e` | 必须位于用户可写根之下；默认作业目录 |
| `join_output` | `-j oe` 合并输出 | bool，默认 true |

> 作业脚本内容经 **stdin** 传入，参数经 `exec.Command` 的**参数数组**传递，杜绝 shell 拼接注入。

### 5.2 作业状态（PBS 字母）

`Q` 排队、`R` 运行、`H` 挂起、`E` 退出中、`B` 已开始、`W` 等待、`T` 转移中、`S` 暂停、`X` 已结束；`qstat -f` 另有 `Exit_status`、`resources_used` 等。

### 5.3 作业标识

格式 `<seq>.<server>`，如 `12345.easemgt`。后端用严格正则 `^[0-9]+(\[[0-9]+\])?\.[A-Za-z0-9_.-]+$` 校验后再交给命令。

---

## 6. 接口设计（全部 `Bearer mgmt`）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/jobs/queues` | 可用队列与资源上限（解析 `qstat -Qf`） |
| POST | `/api/jobs` | 提交作业，body=§5.1 → 返回 `{job_id}` |
| GET | `/api/jobs` | 当前用户作业列表（`qstat -u <u>` 表格解析） |
| GET | `/api/jobs/{id}` | 作业详情（`qstat -f {id}` 解析为键值） |
| GET | `/api/jobs/{id}/log?stream=out\|err&offset=` | 读取输出文件（tail，支持增量 offset） |
| POST | `/api/jobs/{id}/cancel` | `qdel {id}` |
| POST | `/api/jobs/{id}/hold` / `/release` / `/rerun` | `qhold` / `qrls` / `qrerun`（P2） |

`JobInfo`（归一化）：

```json
{
  "id": "12345.easemgt",
  "name": "train",
  "owner": "ushij067",
  "state": "R",
  "queue": "workq",
  "ncpus": 4,
  "mem_gb": 16,
  "walltime": "08:00:00",
  "used_walltime": "00:12:33",
  "submit_time": "2026-09-28T10:00:00",
  "start_time": "2026-09-28T10:01:05",
  "exec_host": "node123/0-3",
  "exit_status": null,
  "output_path": "/share/easemgt/ushij067/train.o12345"
}
```

---

## 7. PBS 命令与解析（面向 18.1，无 JSON）

### 7.1 命令映射

| 操作 | 命令 |
|---|---|
| 提交 | `qsub -N <name> -q <q> -l select=1:ncpus=N:mem=NG -l walltime=HH:MM:SS -d <workdir> -o <out> -e <err> -j oe`（脚本 stdin） |
| 列表 | `qstat -u <user>` |
| 详情 | `qstat -f <jobid>` |
| 队列 | `qstat -Qf` / `qstat -Qf <queue>` |
| 取消 | `qdel <jobid>` |
| 挂起/恢复/重跑 | `qhold` / `qrls` / `qrerun` |

> **18.1 注意**：`qstat -f -F <fmt>` 的格式串在 18.1 上不可靠（实测报 usage），**统一解析 `qstat -f` 的 `key = value` 文本**。

### 7.2 `qstat -f` 解析要点

- 一个作业以 `Job Id: <id>` 开头，其后缩进行为 `    key = value`；
- 多行属性（如 `Variable_List`）以缩进续行，遇到下一个 `key = value`/`Job Id` 结束；
- 关键属性：`Job_Name`、`Job_Owner`（`user@host`）、`job_state`、`queue`、`Resource_List.ncpus`、`Resource_List.mem`、`Resource_List.walltime`、`resources_used.walltime`、`submit_time`、`start_time`、`exec_host`、`Exit_status`、`Output_Path`/`Error_Path`（`host:/path`，需去主机前缀）。
- `qstat -u <user>` 表格列：`Job id Name User Time Use S Queue`，用于快速列表。

> 解析器需容错：未知属性忽略、字段缺失置空、时间按 PBS 格式解析（可保留原串）。

### 7.3 日志读取

- `Output_Path`/`Error_Path` 解析出**本地路径**后，以用户身份 `read`（`sudo -u <user>` 或直接读，需权限校验），支持 `offset` 增量 tail。
- 路径必须落在允许根（用户 home / 配置目录）之下，防目录穿越。
- 作业结束前后均可读（PBS 运行中亦会写输出文件）。

---

## 8. 前端交互

```
登录 → mgmt_token
作业面板（独立于"会话"面板）：
  ├─ 队列选择 + 资源表单（ncpus / mem / walltime / workdir）
  ├─ 脚本编辑框（textarea）   → [提交]
  ├─ 作业列表（id/名称/状态/队列/耗时）[刷新]（可轮询，如 5s）
  │     点击 → 详情（exec_host、起止时间、退出码）
  │     查看日志 → 实时 tail（out/err 切换，自动滚动）
  │     操作 → [取消]（P2: [挂起][恢复][重跑]）
  └─ 无 PBS 客户端：提示"本机未安装 PBS 客户端"
```

- 轮询：列表/运行中作业详情按固定间隔刷新（如 5s），无 WebSocket 需求；后续可改 SSE。

---

## 9. 安全

1. **身份**：全部操作 `sudo -n -u <user>` 执行；作业归属与配额归登录用户。
2. **无注入**：脚本走 stdin；命令行用参数数组；job id 正则校验；不使用 shell。
3. **白名单**：队列必须在 `allowed_queues`；资源数值有上下界；工作目录/输出路径限定在允许根内。
4. **归属校验**：任何 job 操作前用 `qstat -f <id>` 校验 `Job_Owner` 属于当前 username，否则 404（不泄露他人作业是否存在）。
5. **限流**：`max_jobs_per_user`（排队+运行）上限，防止滥用；提交接口简易速率限制。
6. **凭证**：mgmt_token 走 `Authorization` 头；不打印；`sudo -u` 命令行不含机密（脚本经 stdin）。
7. **日志路径**：读日志前做路径归一化 + 前缀校验，防穿越。
8. **权限面**：`sudoers` 现有 `ease ALL=(ALL,!root) NOPASSWD: ALL` 已覆盖 `sudo -u <user>`，无需新增；PBS 命令**不需要** root。

---

## 10. 配置（`config.yaml` 新增 `jobs` 段）

```yaml
jobs:
  enabled: true
  pbs_bin: "/opt/pbs/bin"          # PBS 客户端目录（不在 PATH）
  default_queue: "workq"
  allowed_queues: ["workq"]
  default_ncpus: 1
  max_ncpus: 32                     # 参考 resources_max.ncpus
  default_mem_gb: 4
  max_mem_gb: 128
  max_walltime_hours: 168
  max_jobs_per_user: 20
  log_tail_bytes: 65536
  workdir_allow_under: ["/home", "/share/easemgt"]   # 允许的工作目录/输出根
```

---

## 11. 与交互会话、沙箱的关系

- **互不干扰**：会话面板与作业面板独立；`qsub` 不占用交互会话名额，作业也不进会话列表。
- **沙箱**：作业在计算节点执行，登录节点沙箱**不适用**于作业本体。`qsub` 客户端命令以用户身份在登录节点执行；如需更强约束可选地在用户沙箱内执行 qsub，但会引入与 PBS 认证/网络相关的复杂性，**P1 不做**。
- **凭证复用**：共用 `mgmt_token`（§4），登录一次即可用两个子系统。

---

## 12. 持久性与并发

- 作业由 PBS 持有 → **webshell 重启/发版不影响**。
- 并发：同一用户的多个作业由 PBS 排队；webshell 侧仅需无状态代理。
- 无本地状态需要持久化（不引入 sessiond）。

---

## 13. 失败模式与容错

| 现象 | 处理 |
|---|---|
| `pbs_bin` 不存在 / qstat 不可执行 | 接口返回 503，UI 提示"本机未安装 PBS 客户端" |
| PBS 服务器不可达 | qstat/qsub 命令报错 → 502，透传简要错误（不含敏感信息） |
| qsub 参数被拒（资源超限/队列不允许） | 解析 stderr → 400 + 可读原因 |
| `qstat -f` 解析不到作业 | 404 |
| 输出文件不存在/未生成 | 日志接口返回空 + 提示 |
| 作业 id 非法 | 400（正则不通过） |

---

## 14. 未来扩展

- **作业数组 / 依赖 / `-W` 高级选项**；
- **模板 / 一键提交**（常见软件：abaqus/ansys/altair 等，结合 `/share/apps`）；
- **`qsub -I` 交互式作业**：在计算节点开交互会话并转发 pty（与 `DESIGN_SESSIONS` 的 sessiond 结合，复杂度高，单独立项）；
- **SSE/WebSocket 推送**替代轮询；
- **资源使用图表**（`resources_used` 历史）。

---

## 15. 测试矩阵

| 场景 | 预期 |
|---|---|
| 提交合法作业（echo/sleep） | 返回 job_id，qstat 可见 |
| 队列不在白名单 | 400 |
| ncpus/mem/walltime 越界/非数字 | 400 |
| 脚本含 shell 元字符 | 原样作为脚本内容（经 stdin），无注入 |
| 提交后列表 | 仅显示本人作业 |
| 查看本人作业详情 | 200，字段解析正确 |
| 查看他人作业 id | 404 |
| 取消本人作业 | `qdel` 成功，状态转 `X`/消失 |
| 取消他人作业 | 404 |
| 读取运行中作业日志 | 增量 tail 正确 |
| 日志路径穿越 `../../etc/shadow` | 拒绝 |
| 超过 max_jobs_per_user | 拒绝提交 |
| PBS 客户端缺失 | 503，UI 提示 |
| 解析 `qstat -f` 样本（含多行属性） | 键值正确 |
| webshell 重启后作业 | 仍在（PBS 持有） |

---

## 16. 实施顺序

1. `pbs/` 包：`Client`（`qsub/qstat/qdel/...` 封装 + `sudo -u` 执行 + 超时）、`ParseQstatF`、`ParseQueueF`。
2. `auth`：复用/扩展 mgmt_token（可选 scopes）。
3. `handler/jobs_api.go`：`/api/jobs*` + `requireMgmt` + 归属校验 + 字段校验。
4. `handler/jobs_parse.go`：解析归一化。
5. `config.go` / `config.example.yaml`：`jobs` 段。
6. `main.go`：注册路由。
7. 前端：作业面板（表单/列表/详情/日志/取消）。
8. `go build ./...` + `go test ./...`（解析器单测用真实 `qstat -f` 样本）。
9. 文档同步。

> **前提检查项**：部署主机须有 PBS 客户端（`/opt/pbs/bin` + `/etc/pbs.conf`）；当前登录节点 `10.195.86.188` 满足，`10.195.86.126` 不满足。
