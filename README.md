# Web Shell

一个基于 Web 的远程终端服务：用户通过浏览器登录（AD/LDAPS 强认证）后，可进入交互式终端会话。支持在登录后选择打开**普通 Bash 终端**或 **opencode TUI**，并可自动将共享的 opencode 配置同步到各用户的家目录。

支持 **Linux 沙箱隔离**（user namespace + pivot_root + cgroup + seccomp）：沙箱内进程以宿主真实用户身份运行（保留补充组），解决 NFS 因丢失补充组导致 Permission denied 的问题。

- 语言：Go
- 终端：xterm.js（浏览器端）+ `creack/pty`（服务端伪终端）
- 认证：AD（LDAPS）+ 一次性 token
- 通信：WebSocket（JSON 消息协议）

## 功能特性

- **AD 登录认证**：通过 LDAPS 连接域控，支持账号名或邮箱登录；可开启密码强认证（用户 DN + 密码 bind）。
- **一次性 token**：登录成功签发一次性 token（默认 5 分钟有效），WebSocket 升级前校验并消费，防重放。
- **会话模式选择**：登录成功后，若启用了 opencode，前端展示「打开 Bash / 打开 OpenCode」选择面板；用户选择后才建立终端连接。
- **opencode 集成**：选择 opencode 后，以登录用户身份在其家目录启动 opencode TUI；退出 opencode 即完全结束会话（不退回 bash）。
- **配置同步**：登录时可将共享的 opencode 配置（`skills/` 与 `opencode.jsonc`）同步到用户 `~/.config/opencode`。用户首次初始化后（已存在 `skills` 目录）即跳过同步，保留其已有配置。
- **以目标用户身份运行**：沙箱模式（`sandbox.enabled=true`）下通过 `sudo -n -u root sandbox-init` 启动沙箱，沙箱内进程以**真实用户身份**（含补充组）运行；非沙箱模式（`sandbox.enabled=false`）退回传统 `sudo -n -u <user> -i bash` 切换登录用户，确保文件归属与权限正确。
- **终端字号调整**：终端工具条提供 `A-`/`A+` 按钮调整字号（8–32px，步进 2px），选择持久化到 `localStorage`，下次登录自动恢复。
- **终端主题切换**：工具条下拉框内置 8 个流行主题（VS Code Dark、Dracula、Monokai、Nord、Solarized Dark、One Dark、Tokyo Night、GitHub Light），实时切换并持久化，下次登录自动恢复。
- **刷新保持会话**：页面刷新或短暂断线后自动重连到同一终端，正在运行的进程不丢失（详见「会话模式与会话行为」）。
- **沙箱隔离**（user namespace 方案 A）：通过 `sudo -n -u root sandbox-init` 启动沙箱，mount/PID/net namespace + pivot_root + 只读 rootfs + cgroup + seccomp；沙箱内进程以**宿主真实用户身份**运行（保留补充组，非 root、非 userns root），解决 NFS 丢失补充组导致的 Permission denied 问题。
- **bind_mounts 支持**：NFS 等宿主已挂载路径可 bind 进沙箱（简单列表或 source/target 对象形式）。
- **/etc/hosts 映射**：`bind_hosts` 可将宿主机 `/etc/hosts` 映射进沙箱，解决沙箱内主机名解析。
- **沙箱内体验**：中文正常（`LANG=en_US.utf8`）、真实用户名提示符、vim（rootfs 内）、opencode（rootfs 内 + 共享配置 bind）均可用。

## 目录结构

```
.
├── main.go                  # 入口：配置加载、路由注册、静态资源托管、服务启动
├── static_embed.go          # //go:embed 内嵌 static/ 到二进制
├── config.example.yaml      # 配置示例（复制为 config.yaml 使用）
├── DEPLOY.md                # 部署指南（Ubuntu + RHEL，含沙箱部署）
├── go.mod / go.sum          # Go 模块依赖
├── auth/                    # AD 认证、token、配置加载
│   ├── ad.go                # AD(LDAPS) 连接与认证
│   ├── auth.go              # 认证流程（正则/非root/系统存在/AD/token）
│   ├── config.go            # config.yaml 解析与默认值
│   └── token.go             # 一次性 token 存储（内存、线程安全）
├── handler/                 # HTTP 与 WebSocket 处理器
│   ├── login.go             # POST /api/login
│   ├── ws.go                # /ws WebSocket（token 校验、模式选择、会话启动/恢复）
│   └── session_hub.go       # 会话仓库（刷新后恢复同一终端、过期回收）
├── pty/
│   └── session.go           # pty 会话管理、sudo 启动、opencode 配置同步
├── sandbox/                 # Linux 沙箱隔离（方案 A：真实用户身份 + 保留补充组）
│   ├── sandbox.go           # 沙箱启动逻辑（enterCgroup、buildRootFS、seccomp、降权）
│   ├── userns_init.c        # C 辅助程序（单线程 unshare/ns 创建，root 执行，需 gcc 编译）
│   └── main.go              # sandbox-init 入口（--child 分支等）
├── protocol/
│   └── message.go           # WebSocket 消息编解码
├── static/                  # 前端资源（内嵌）
│   ├── index.html           # 登录面板、模式选择面板、终端工具条与终端容器
│   ├── app.js               # 前端逻辑（登录、WS、xterm、字号/主题、会话恢复）
│   └── css/style.css        # 样式
└── opencode/                # opencode 相关资源（共享配置源可参考）
    ├── skills/              # 共享 skills
    ├── opencode.jsonc       # opencode 配置
    └── package.json         # opencode 依赖
```

## 编译构建

需要 Go 1.26.5 或更高版本，以及 gcc（用于编译 C 辅助程序 `userns_init.c`）。

```bash
# 主程序
go build -o /opt/webshell_sandbox/webshell .
# 沙箱启动器（Go）
go build -o /opt/webshell_sandbox/sandbox-init ./sandbox
# C 辅助（单线程 unshare/ns 创建）
gcc -O2 -static -o /opt/webshell_sandbox/userns-init sandbox/userns_init.c
# rootfs 构建（见 DEPLOY.md / scripts/build_rootfs.sh）
```

前端静态资源已通过 `//go:embed` 内嵌进二进制，无需单独部署静态文件。

## 配置说明

服务通过 `config.yaml` 配置（可执行文件同级目录，或通过 `-config` / 环境变量 `CONFIG_PATH` 指定）。参考 `config.example.yaml`。

```yaml
# 服务监听地址与端口
listen: "0.0.0.0:8080"

ad:
  server: "ldaps.example.com:636"  # LDAPS 服务器 host:port
  manager_dn: "MANAGER@DOMAIN"     # manager 账号 DN
  manager_password: "CHANGE_ME"    # manager 账号密码
  search_dn: "DC=example,DC=com"   # 搜索基准 DN
  tls_insecure: true               # 是否跳过 TLS 证书校验
  timeout: 5                       # AD 连接/操作超时（秒）

require_password: true             # true=密码强认证；false=仅目录查询
token_ttl: 300                     # 一次性 token 有效期（秒）

# opencode 模式：登录后由用户选择进入 opencode TUI 或 bash
opencode:
  enabled: true                    # true=展示「打开 Bash / 打开 OpenCode」选择面板
  path: "/opt/opencode/bin/opencode"  # opencode 可执行文件路径
  sync_from: "/opt/opencode-config"    # 共享配置源目录（含 skills/ 与 opencode.jsonc）
  overwrite: true                  # 首次初始化时是否覆盖 opencode.jsonc

# Linux 沙箱（sandbox-init 由 sudoers 以 root 启动，作为唯一提权入口）
sandbox:
  enabled: false                  # true 启用沙箱；false 退回传统 sudo 切用户模式
  init_path: "/opt/webshell_sandbox/sandbox-init"  # sandbox-init 二进制绝对路径
  rootfs_path: "/opt/runner/rootfs/base"   # base rootfs 绝对路径（只读新根 bind）
  network: "none"                 # none|loopback|full（缺省 none，即无网络）
  tmp_size: "256M"                # /tmp tmpfs 大小（缺省 256M）
  nofile_limit: 4096              # RLIMIT_NOFILE（缺省 4096）
  core_dump: false                # true 允许 core dump；false（缺省）RLIMIT_CORE=0
  max_fsize: "1G"                 # RLIMIT_FSIZE（缺省 1G）
  seccomp: true                   # true（缺省）安装 seccomp 黑名单；false 关闭
  proc_hidepid: true              # true（缺省）以 hidepid=2 挂载 /proc；false 关闭
  cgroup_root: "/sys/fs/cgroup/webshell_sandbox"  # cgroup v2 根目录
  # bind 宿主已挂载路径（含 NFS 挂载点）到沙箱内路径
  bind_mounts:
    - "/easeshare/SH/Method2"     # 简单形式：source 与 target 相同（自动匹配宿主已挂载路径）
    - "/easeshare/SIMU3/Sys_Run"  # 亦兼容对象形式：{source: "...", target: "..."}
  bind_hosts: true                # 映射宿主机 /etc/hosts 到沙箱（解决沙箱内主机名解析）
```

> 配置同步仅在用户首次初始化（`~/.config/opencode/skills` 不存在）时执行；一旦 `skills` 目录已存在，则跳过全部同步（opencode.jsonc 与 skills 均不覆盖），保留用户已有配置。`overwrite` 仅在首次初始化时生效。

**sandbox 配置块字段说明**：

- `enabled`：`true` 启用沙箱；`false`（缺省）退回传统 `sudo -n -u <user> -i bash` 模式。
- `init_path` / `rootfs_path`：sandbox-init 二进制与 base rootfs 的绝对路径。
- `network`：`none`（缺省，无网络）/ `loopback`（仅回环）/ `full`（共享宿主网络，NFS 可访问、opencode 可联网）。
- `memory_max`（如 `"512M"`，空则不限制）、`cpu_quota`（如 `"50000 100000"`，空则不限制）、`pids_max`（`0` 不限制）：cgroup 资源限制。
- `tmp_size`：沙箱内 `/tmp` tmpfs 大小（缺省 `256M`）。
- `nofile_limit` / `core_dump` / `max_fsize`：rlimit 相关（best-effort 设置，失败不阻断启动）。
- `seccomp`：安装 seccomp 黑名单（拦截 mount/unshare 等）；`proc_hidepid`：以 `hidepid=2` 挂载 `/proc`。
- `cgroup_root`：cgroup v2 根目录（需 root 预创建并启用 cpu/memory/pids 控制器）。
- `bind_mounts`：宿主已挂载路径（含 NFS）bind 进沙箱。简单形式字符串列表（source=target）；也可用对象形式 `{source: "...", target: "..."}` 支持 source 与 target 不同。
- `bind_hosts`：将宿主机 `/etc/hosts` 映射进沙箱，解决沙箱内主机名解析。

### 配置加载优先级

1. `-config` 命令行 flag
2. 环境变量 `CONFIG_PATH`
3. 默认 `./config.yaml`

若显式设置环境变量 `PORT`，则优先于 `listen` 生效。

## 运行

```bash
# 直接运行
./webshell

# 指定配置
./webshell -config /path/to/config.yaml
# 或
CONFIG_PATH=/path/to/config.yaml ./webshell
```

默认监听 `0.0.0.0:8080`。

### systemd 托管（推荐）

服务由持久化 systemd 单元 `/etc/systemd/system/webshell.service` 托管，支持开机自启与 `systemctl` 标准管理：

```ini
[Unit]
Description=Web Shell - AD authenticated web terminal with opencode support
After=network.target

[Service]
Type=simple
User=ease
WorkingDirectory=/opt/webshell
Environment=CONFIG_PATH=/opt/webshell/config.yaml
ExecStart=/opt/webshell/webshell
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable webshell   # 开机自启
systemctl start webshell    # 启动
systemctl restart webshell  # 重启
systemctl status webshell   # 查看状态
```

> 部署新二进制时需先 `systemctl stop webshell`，再替换 `/opt/webshell/webshell`，最后 `systemctl start webshell`（避免 `Text file busy`）。

> 早期曾用 `systemd-run --unit=webshell --collect` 创建 transient 单元（停止后自动清理、重启需重新创建），现已改用上面的持久化单元（`enabled` 开机自启）。

## 沙箱部署

启用 Linux 沙箱隔离时，除常规部署外还需：构建 3 个二进制（webshell / sandbox-init / userns-init）、构建 rootfs、配置 `config.yaml` 的 `sandbox` 块、配置 sudoers + cgroup + systemd 单元，最后启动验证。**详细部署见 DEPLOY.md，含 Ubuntu 与 RHEL（关闭 SELinux）**，此处仅列概要：

1. **构建 3 个二进制**：`go build` 主程序与 `sandbox-init`，`gcc -O2 -static` 编译 `userns-init`（见「编译构建」）。
2. **构建 rootfs**：在目标服务器上运行 `/opt/webshell_sandbox/scripts/build_rootfs.sh`（需 root），构建到 `/opt/runner/rootfs/base`（约 500–600MB）。
3. **配置 config.yaml**：启用 `sandbox.enabled: true`，设置 `init_path`、`rootfs_path`、`network`、`bind_mounts`、`bind_hosts` 等（见「配置说明」）。
4. **sudoers + cgroup + systemd**：添加沙箱 sudoers 规则；预创建 `/sys/fs/cgroup/webshell_sandbox` 并启用 cpu/memory/pids 控制器；配置 `webshell-sandbox.service`（端口可配，示例 8090）。
5. **启动验证**：启动 systemd 服务，浏览器 AD 登录后，沙箱内 `id` 应显示真实用户名（uid/gid/补充组与宿主机一致），vim、opencode、NFS bind 路径可用。

> RHEL 差异：需关闭 SELinux、适配 `/usr/lib64` 库路径的 rootfs 脚本、firewalld 放行端口，详见 DEPLOY.md。

## 服务运行用户的系统权限

服务以非特权用户（本文档示例为 `ease`）运行，通过 `sudo -n -u <user> -i bash` 切换到登录用户来启动会话，并将共享 opencode 配置同步到用户家目录。因此服务运行用户需要以下系统权限配置：

### 1. sudoers 权限（核心）

服务必须以目标登录用户身份执行命令，需为服务用户配置**无密码 sudo 切换到除 root 外任意用户**：

```bash
# /etc/sudoers.d/ease
ease  ALL=(ALL,!root)  NOPASSWD: ALL
```

> **必须写成 `(ALL,!root)` 而非 `(ALL:ALL,!root)`**：在 sudo 1.9.9 下，`(ALL:ALL,!root)` 的 `!root` 排除对 `sudo -u root` 不生效，会导致服务用户仍能切到 root（严重提权风险）；`(ALL,!root)` 能正确排除 root。

该规则同时服务于两条路径：
- **会话启动**：`sudo -n -u <username> -i bash`（切换登录用户）；
- **配置同步**：`sudo -n -u <username> -i bash -c '...cp <sync_from>/skills ...'`（以用户身份复制，使文件归用户所有，避免服务用户无 chown 权限）。

**沙箱模式的 sudoers 规则（提权入口）**：启用沙箱时，额外放行一条**精确到路径**的 sudo 规则，允许服务用户仅以 root 运行 `sandbox-init`（最短提权面）：

```bash
# /etc/sudoers.d/webshell-sandbox
ease ALL=(root) NOPASSWD: /opt/webshell_sandbox/sandbox-init
```

> - 沙箱二进制经 `sudo -n -u root /opt/webshell_sandbox/sandbox-init`（**无 `-i`**）以 root 启动，内部完成 unshare/pivot_root 后降权到真实用户（含补充组）；
> - `/opt/webshell_sandbox` 目录必须 **root:root 0755**，防止服务用户替换 `sandbox-init` 二进制提权 root；
> - 原 `ease ALL=(ALL,!root) NOPASSWD: ALL` 规则可保留（用于非沙箱会话及配置同步），与沙箱规则互不冲突；
> - cgroup 根目录（如 `/sys/fs/cgroup/webshell_sandbox`）需 root 预创建，并启用 cpu/memory/pids 控制器；systemd 开机自启见 DEPLOY.md。

### 2. 服务自身目录

| 路径 | 权限要求 | 说明 |
| ---- | -------- | ---- |
| 二进制 `/opt/webshell/webshell` | 服务用户可读、可执行 | `chown ease:ease` |
| 配置 `/opt/webshell/config.yaml` | 服务用户可读（含 AD 密码，建议 `0600`） | `chown ease:ease; chmod 600` |
| 工作目录 `/opt/webshell` | 服务用户可读、可进入 | |

### 3. opencode 相关资源（供服务用户读取）

| 路径 | 权限要求 |
| ---- | -------- |
| opencode 二进制 `/opt/opencode/bin/opencode` | 所有用户可执行（含服务用户） |
| 配置源目录 `/opt/opencode-config`（含 `skills/` 与 `opencode.jsonc`） | 服务用户可读（`sync_from` 指向） |

### 4. 服务用户 shell 说明

服务用户的登录 shell 通常设置为 `/usr/sbin/nologin`（如 `ease`）。这**不影响**功能：会话与同步均通过 `sudo -i` 以目标登录用户身份执行，不依赖服务用户自身的 shell。

## 认证流程

`POST /api/login` 处理流程（`auth/auth.go`）：

1. 校验用户名/邮箱格式（正则）；
2. 拒绝 root 登录；
3. `require_password` 开启时校验密码非空；
4. 解析标识：账号名直接使用；邮箱经 AD 反查得到 `sAMAccountName`/DN/UAC；
5. AD 认证：密码强认证（用户 DN + 密码 bind）或目录查询模式；
6. 校验系统用户存在并取 HomeDir；
7. 签发一次性 token。

`/ws` 升级前校验并消费 token（防重放），再以目标用户启动会话。

## 会话模式与会话行为

WebSocket 升级时通过 `?mode=` 查询参数决定会话类型（`handler/ws.go`）：

- `mode=opencode` 且 opencode 已启用 → 先同步配置，再以该用户启动 opencode TUI；
- 其余情况（未传 / `mode=bash` / opencode 未启用）→ 直接进入该用户的 bash。

**沙箱模式启动**：启用沙箱（`sandbox.enabled=true`）时，会话通过 `sudo -n -u root /opt/webshell_sandbox/sandbox-init <args>`（无 `-i`）启动：root 阶段完成 enterCgroup → 预创建 rootfs 挂载点 → resolveSupplementaryGroups → exec `userns-init`（C，root 单线程）执行 `unshare(NS|PID|NET)` → `make-rprivate` → fork → exec `sandbox-init --child`（root）；子进程 `buildRootFS`（bind rootfs/home/NFS/dev/proc）→ `pivot_root` → rlimit(best-effort) → seccomp → `upLoopback` → `dropToUser`（setgroups 主组+补充组 → setgid → setuid）→ exec bash/opencode。沙箱内进程是**宿主真实用户身份**（含补充组，非 root、非 userns root），解决 NFS 因丢失补充组导致 Permission denied 的问题。非沙箱模式（`sandbox.enabled=false`）退回传统 `sudo -n -u <user> -i bash`。

**退出行为**：

- **bash 模式**：退出 bash 即结束会话，前端回到登录界面；
- **opencode 模式**：退出 opencode TUI 即完全结束会话（不退回 bash），前端回到登录界面。

**刷新保持会话**：页面刷新或短暂断线时，WebSocket 连接断开但 pty 会话会保留在服务端会话仓库（`handler/session_hub.go`）中一段时间（默认 5 分钟），前端把会话标识存入 `sessionStorage`，刷新后自动通过 `/api/resume` 探测并重连到同一终端——正在运行的进程（vim、命令等）不丢失。关闭标签页后 `sessionStorage` 清除，会话超时后被巡检自动回收。

## WebSocket 协议

`/ws?token=<token>&mode=<bash|opencode>` 升级后，消息为 JSON 信封（`protocol/message.go`）：

| 类型     | 方向         | 说明                                             |
| -------- | ------------ | ------------------------------------------------ |
| `input`  | 客户端→服务端 | 写入 pty 的输入字节（JSON 字符串）               |
| `resize` | 客户端→服务端 | 设置终端尺寸 `{cols, rows}`                       |
| `output` | 服务端→客户端 | pty 输出字节（base64 编码后放入字符串）          |
| `close`  | 服务端→客户端 | 会话结束/错误 `{reason}`                          |
| `session`| 服务端→客户端 | 会话标识 `{id}`（刷新后用于恢复同一终端）         |

恢复会话时前端改为连接 `/ws?session=<id>&username=<user>`（无需一次性 token），服务端据此重新附着到同一 pty 会话。

## 测试

```bash
go test ./...
```

当前测试覆盖 `auth` 包（配置加载默认值、必填项校验、YAML 解析等）。

## 安全说明

- `config.yaml` 含 AD manager 密码，建议权限设为 `0600`；
- 日志不会记录密码原文；含 `@` 的邮箱登录名统一记为 `<email>` 占位；
- 会话以目标用户运行（沙箱模式下为沙箱内真实用户身份，非沙箱模式为 `sudo -n -u <user> -i bash`），服务用户本身无该用户家目录访问权限时不受影响（工作目录设为 `/`，由 `sudo -i` / 沙箱切到用户家目录）；
- 会话标识（sessionID）为 32 字节高熵随机值，作为恢复会话的 bearer 凭证并绑定用户名；断线保留设 5 分钟超时，超时由巡检自动回收，避免会话无限存活。

**沙箱安全**（启用沙箱时）：

- `/opt/webshell_sandbox` 目录属主必须 **root:root 0755**，防止服务用户替换 `sandbox-init` / `userns-init` 二进制提权 root；sudoers 仅放行精确路径 `sandbox-init`；
- rootfs 内无 setuid 二进制（如 `su` 已去除 setuid 位），沙箱内 `/etc` 只读（pivot_root + 只读 rootfs）；
- seccomp 拦截 mount/unshare/setns 及新版挂载 API 等（沙箱内不可再创建命名空间/挂载）；
- cgroup 限制 memory/cpu/pids 生效，防止资源滥用；
- `/proc` 以 `hidepid=2` 挂载 + PID namespace，沙箱内 `ps` 只见本会话进程；
- 沙箱内进程为宿主真实用户身份（非 root、非 userns root），文件归属与权限正确，且不越过用户自身权限。
