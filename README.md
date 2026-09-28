# Web Shell

一个基于 Web 的远程终端服务：用户通过浏览器登录（AD/LDAPS 强认证）后，可进入交互式终端会话。支持在登录后选择打开**普通 Bash 终端**、**opencode TUI** 或 **codex TUI**，并可自动将共享的 opencode / codex 配置同步到各用户的家目录。

支持 **Linux 沙箱隔离**（user namespace + pivot_root + cgroup + seccomp）：沙箱内进程以宿主真实用户身份运行（保留补充组），解决 NFS 因丢失补充组导致 Permission denied 的问题。

- 语言：Go
- 终端：xterm.js（浏览器端）+ `creack/pty`（服务端伪终端）
- 认证：AD（LDAPS）+ 一次性 token
- 通信：WebSocket（JSON 消息协议）

## 功能特性

- **AD 登录认证**：通过 LDAPS 连接域控，支持账号名或邮箱登录；可开启密码强认证（用户 DN + 密码 bind）。
- **一次性 token**：登录成功签发一次性 token（默认 5 分钟有效），WebSocket 升级前校验并消费，防重放。
- **会话模式选择**：登录成功后，若启用了 opencode 或 codex，前端展示「打开 Bash / 打开 OpenCode / 打开 Codex」选择面板；用户选择后才建立终端连接。
- **opencode 集成**：选择 opencode 后，以登录用户身份在其家目录启动 opencode TUI；退出 opencode 即完全结束会话（不退回 bash）。
- **codex 集成**：选择 codex 后，以登录用户身份在其家目录启动 codex TUI（经 `sudo -n -E -u <user> -i bash -c 'cd <home> && <codexPath>'`）；退出 codex 即完全结束会话（不退回 bash）。
- **配置同步**：登录时可将共享的 opencode 配置（`skills/` 与 `opencode.jsonc`）同步到用户 `~/.config/opencode`。用户首次初始化后（已存在 `skills` 目录）即跳过同步，保留其已有配置。此外**每次启动都会单独检查 `~/.config/opencode/AGENTS.md`**：缺失且源 `skills` 同目录的 `AGENTS.md` 存在时才复制（独立于整体同步跳过标记、已存在不覆盖、源缺失静默跳过、与 `overwrite` 无关），保证全局指令始终可用。**agent 目录（策略 A）**：源 `/share/apps/.opencode-config/agent/` 存在且**非空**、目标 `~/.config/opencode/agent/` 不存在时才整体复制；目标已有则跳过（保留用户自定义 agent）；源目录为空时不创建目标空目录；独立于整体跳过标记、与 `overwrite` 无关。**同步命令必须保持单行（用分号 `;` 分隔多条语句）**：`syncConfigCommand`（opencode）生成的同步命令若用换行 `\n` 分隔，经 `sudo -i bash -c` 执行时换行会被丢弃，`fi` 与 `if` 粘连成 `fiif` 报 bash 语法错误、同步静默失败（仅 log 不阻塞，用户表现为「配置没同步」）。
- **codex 配置同步**：登录时可将共享的 codex 配置（`/share/apps/.codex-config/`，含 `config.toml`）同步到用户 **`~/.codex`**（注意不是 `~/.config/codex`）。用户首次初始化后（已存在 `config.toml`）即跳过同步，保留其已有配置；**白名单同步**：只复制 `config.toml` 与 `skills/`，绝不同步任何运行态/敏感文件（sqlite/jsonl/日志/会话等）。此外**每次启动都会单独检查 `~/.codex/AGENTS.md`**：缺失且源 `AGENTS.md` 存在时才复制（独立于整体跳过标记、已存在不覆盖、源缺失静默跳过、与 `overwrite` 无关）。**agents 目录（策略 A）**：源 `/share/apps/.codex-config/agents/` 存在且**非空**、目标 `~/.codex/agents/` 不存在时才整体复制；目标已有则跳过（保留用户自定义 agent）；源目录为空时不创建目标空目录；独立于整体跳过标记、与 `overwrite` 无关。**同步命令必须保持单行（用分号 `;` 分隔多条语句）**：`syncCodexCommand`（codex）生成的同步命令若用换行 `\n` 分隔，经 `sudo -i bash -c` 执行时换行会被丢弃，`fi` 与 `if` 粘连成 `fiif` 报 bash 语法错误、同步静默失败（仅 log 不阻塞，用户表现为「配置没同步」）。
- **以目标用户身份运行**：沙箱模式（`sandbox.enabled=true`）下通过 `sudo -n -u root sandbox-init` 启动沙箱，沙箱内进程以**真实用户身份**（含补充组）运行；非沙箱模式（`sandbox.enabled=false`）退回传统 `sudo -n -u <user> -i bash` 切换登录用户，确保文件归属与权限正确。
- **终端字号调整**：终端工具条提供 `A-`/`A+` 按钮调整字号（8–32px，步进 2px），选择持久化到 `localStorage`，下次登录自动恢复。
- **终端主题切换**：工具条下拉框内置 8 个流行主题（VS Code Dark、Dracula、Monokai、Nord、Solarized Dark、One Dark、Tokyo Night、GitHub Light），实时切换并持久化，下次登录自动恢复。
- **刷新保持会话**：页面刷新或短暂断线后自动重连到同一终端，正在运行的进程不丢失（详见「会话模式与会话行为」）。
- **沙箱隔离**（user namespace 方案 A）：通过 `sudo -n -u root sandbox-init` 启动沙箱，mount/PID/net namespace + pivot_root + 只读 rootfs + cgroup + seccomp；沙箱内进程以**宿主真实用户身份**运行（保留补充组，非 root、非 userns root），解决 NFS 丢失补充组导致的 Permission denied 问题。
- **bind_mounts 支持**：NFS 等宿主已挂载路径可 bind 进沙箱（简单列表或 source/target 对象形式）。
- **/etc/hosts 映射**：`bind_hosts` 可将宿主机 `/etc/hosts` 映射进沙箱，解决沙箱内主机名解析。
- **/etc/resolv.conf 映射**：`bind_resolv` 可将宿主机 `/etc/resolv.conf` 映射进沙箱，解决沙箱内域名无法解析（`network: full` 时生效最佳）。
- **代理支持**：`http_proxy` / `https_proxy` / `no_proxy` 非空时注入沙箱进程环境变量（小写+大写两套），沙箱内网络请求可走代理（需 `network: full` 沙箱有外部网络）。
- **沙箱内体验**：中文正常（`LANG=en_US.utf8`）、真实用户名提示符、vim（rootfs 内）、opencode / codex（rootfs 内 + 共享配置同步）均可用。

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
├── scripts/                 # 部署脚本
│   └── build_rootfs.sh      # 构建沙箱 rootfs（须在目标服务器上运行）
└── opencode/                # opencode 相关资源（共享配置源可参考）
    ├── skills/              # 共享 skills
    ├── opencode.jsonc       # opencode 配置
    └── package.json         # opencode 依赖
```

## 编译构建

需要 Go 1.26.5 或更高版本，以及 gcc（用于编译 C 辅助程序 `userns_init.c`）。

> **⚠️ 必须用 `CGO_ENABLED=1` 构建 `webshell` 与 `sandbox-init`**：若 AD 用户经
> sssd/winbind 解析（`/etc/nsswitch.conf` 的 `passwd:` 含 `sss`），静态链接（Go 默认
> `CGO_ENABLED=0`）的二进制只读 `/etc/passwd`，**无法解析 AD 用户**，登录会报
> `user not provisioned on this host`。`CGO_ENABLED=1` 使 `os/user` 经 libc NSS 解析；
> 因此运行时需 glibc（标准系统自带），构建机需 gcc + libc6-dev。

```bash
# 主程序（沙箱模式必需 CGO_ENABLED=1）
CGO_ENABLED=1 go build -o /opt/webshell_sandbox1/webshell .
# 沙箱启动器（Go）
CGO_ENABLED=1 go build -o /opt/webshell_sandbox1/sandbox-init ./sandbox
# C 辅助（单线程 unshare/ns 创建）
gcc -O2 -static -o /opt/webshell_sandbox1/userns-init sandbox/userns_init.c
# rootfs 构建（见 DEPLOY.md / scripts/build_rootfs.sh）
```

> 部署目录可自定义（本文档以 `/opt/webshell_sandbox1` 为例）：若更换部署目录，需同步修改 `config.yaml` 的 `init_path`、sudoers 精确路径规则、systemd 单元，并注意 `userns_init_path` 留空时自动推导为 sandbox-init 同目录（见「配置说明」）。

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
  path: "/share/apps/.opencode/bin/opencode"  # opencode 可执行文件路径
  sync_from: "/share/apps/.opencode-config"   # 共享配置源目录（含 skills/ 与 opencode.jsonc）
  overwrite: true                  # 首次初始化时是否覆盖 opencode.jsonc

> **api_key 必须用 `{env:XXX_API_KEY}` 引用**：opencode 配置（`opencode.jsonc`）中的
> `apiKey` 必须写为 `{env:XXX_API_KEY}` 形式（如 `"apiKey": "{env:YANFENG_API_KEY}"`）。
> Web Shell 会读取该引用，若用户 `~/.auth.json` 缺失对应 api_key，则在 opencode 启动前
> 弹窗让用户补全（见「WebSocket 协议」的 `auth-request`/`auth-response`）。**硬编码 key
> 将无法触发补全机制**，且会随配置同步泄露给所有用户，请勿硬编码。

# codex 模式：登录后由用户选择进入 codex TUI 或 bash
codex:
  enabled: true                    # true=展示「打开 Bash / 打开 Codex」选择面板
  path: "/share/apps/.codex-standalone/codex"  # codex 可执行文件路径
  sync_from: "/share/apps/.codex-config"       # 共享配置源目录（同步到 ~/.codex，含 config.toml）
  overwrite: true                  # 以 ~/.codex/config.toml 存在性标记为主，overwrite 影响甚微

> **codex 的 api_key 必须用 `env_key` 引用**：codex 配置（`~/.codex/config.toml`）中的
> `model_providers.<id>.env_key = "XXX_API_KEY"` 命名一个环境变量，codex 运行时从其取值
> 作为 API Key（如 `env_key = "YANFENG_API_KEY"`）。Web Shell 会读取该引用，若用户
> `~/.auth.json` 缺失对应 api_key，则在 codex 启动前弹窗让用户补全（复用
> `auth-request`/`auth-response` 机制）。**硬编码 key 将无法触发补全机制**，且会随配置
> 同步泄露给所有用户，请勿硬编码。

# Linux 沙箱（sandbox-init 由 sudoers 以 root 启动，作为唯一提权入口）
sandbox:
  enabled: false                  # true 启用沙箱；false 退回传统 sudo 切用户模式
  init_path: "/opt/webshell_sandbox1/sandbox-init"  # sandbox-init 二进制绝对路径
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
    # 也可 bind 系统/共享目录：如 /etc/ssl/certs（宿主 CA 证书，沙箱内 HTTPS 必需）、
    # /share/apps/.opencode 与 /share/apps/.codex-standalone（opencode/codex 二进制版本跟随宿主）
    - "/etc/ssl/certs"            # 宿主 CA 证书目录（沙箱内 opencode/codex 联网必需）
    - "/var/lib/sss/pipes"        # sssd NSS 应答 socket（沙箱内按名解析 AD 用户/组）
    - "/var/lib/sss/mc"           # sssd 内存缓存（passwd/group/initgroups）
  bind_hosts: true                # 映射宿主机 /etc/hosts 到沙箱（解决沙箱内主机名解析）
  bind_resolv: true               # 映射宿主机 /etc/resolv.conf 到沙箱（解决沙箱内域名解析）
  # userns_init_path: ""          # userns-init 绝对路径；留空自动推导为 sandbox-init 同目录下的 userns-init
  http_proxy: ""                     # http 代理地址（如 "http://proxy.example.com:8080"）；非空时注入沙箱环境变量 http_proxy/HTTP_PROXY
  https_proxy: ""                    # https 代理地址（如 "http://proxy.example.com:8080"）；非空时注入沙箱环境变量 https_proxy/HTTPS_PROXY
  no_proxy: ""                       # 不使用代理的主机列表（如 "localhost,127.0.0.1"）；非空时注入沙箱环境变量 no_proxy/NO_PROXY
```

> 配置同步仅在用户首次初始化（`~/.config/opencode/skills` 不存在）时执行；一旦 `skills` 目录已存在，则跳过全部同步（opencode.jsonc 与 skills 均不覆盖），保留用户已有配置。`overwrite` 仅在首次初始化时生效。
>
> 与整体同步不同，**AGENTS.md 每次启动都会单独检查**：缺失且源 `<sync_from>/AGENTS.md` 存在时才复制到 `~/.config/opencode/AGENTS.md`；独立于整体跳过标记（即使 skills 已存在导致整体同步跳过，AGENTS.md 仍照常检查）；已存在不覆盖（保留用户编辑）；源缺失静默跳过；与 `overwrite` 无关。
>
> **agent 目录同理（策略 A）**：源 `<sync_from>/agent/` 存在且**非空**、目标 `~/.config/opencode/agent/` 不存在时才整体复制；目标已有则跳过（保留用户自定义 agent）；源目录为空时不创建目标空目录；独立于整体跳过标记、与 `overwrite` 无关。
>
> codex 配置同步同理：仅在用户首次初始化（`~/.codex/config.toml` 不存在）时执行；一旦 `config.toml` 已存在则整体跳过，保留用户已有配置。采用**白名单同步**：只复制 `config.toml` 与 `skills/`，绝不同步任何运行态/敏感文件（源目录即使混入 `*.sqlite`/`*.jsonl`/日志/会话等也不会复制）。`overwrite` 对 codex 影响甚微（以 config.toml 存在性标记为主）。codex 的 **AGENTS.md 同理每次启动单独检查** `~/.codex/AGENTS.md`（缺失且源存在才补、独立于整体跳过、已存在不覆盖、源缺失静默跳过、与 `overwrite` 无关）。codex 的 **agents 目录同理（策略 A）**：源 `<sync_from>/agents/` 存在且**非空**、目标 `~/.codex/agents/` 不存在时才整体复制；目标已有则跳过；源目录为空时不创建目标空目录；独立于整体跳过标记、与 `overwrite` 无关。
>
> **同步命令必须保持单行（用分号 `;` 分隔多条语句）**：opencode 的 `syncConfigCommand` 与 codex 的 `syncCodexCommand` 生成的同步命令必须**完全单行**——命令经 `sudo -n -u <user> -i bash -c '<cmd>'` 执行时，**换行 `\n` 会被吞掉**（`fi\nif` 粘连成 `fiif`），bash 报 `syntax error near unexpected token 'then'`，同步静默失败（仅 log 不阻塞，用户表现为「配置没同步」）。多条语句务必用分号 `;` 分隔，**切勿改回多行/换行写法**（实现/部署约束，改动前请留意）。

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
- `bind_resolv`：将宿主机 `/etc/resolv.conf` 映射进沙箱，解决沙箱内域名解析。建议 `network: full`（共享宿主网络栈）时开启，此时宿主 DNS（如 `127.0.0.53` systemd-resolved stub）在沙箱内可达。

**沙箱内 HTTPS 与 CA 证书**：沙箱 rootfs 可能缺少宿主 CA 证书（`/etc/ssl/certs`），会导致沙箱内所有 HTTPS 请求失败——典型现象：codex 报 **"waiting for network"**、curl 报 `(77) error setting certificate file`。修复方式：在 `sandbox.bind_mounts` 中加入 `/etc/ssl/certs`（source=target），把宿主 CA 证书目录 bind 进沙箱。**这是沙箱内 opencode/codex 正常联网的必要配置**，缺省（未 bind）时沙箱内 HTTPS 不可用。

- `userns_init_path`：userns-init 可执行文件绝对路径。**留空（缺省）时自动推导为 sandbox-init 同目录下的 `userns-init`**，无需额外配置；仅在需要将 userns-init 放到其他位置时显式指定。
- `http_proxy` / `https_proxy`：http/https 代理地址（如 `"http://proxy.example.com:8080"`），非空时注入沙箱进程环境变量（同时设置小写与大写两种形式，如 `http_proxy` / `HTTP_PROXY`）。仅 `network: full` 时沙箱有外部网络，代理才能生效。
- `no_proxy`：不使用代理的主机/域名列表（逗号分隔，如 `"localhost,127.0.0.1"`），非空时注入沙箱进程环境变量（同时设置 `no_proxy` / `NO_PROXY`）。

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

> 上面的单元为通用模板（`/opt/webshell`、端口 8080）；**生产示例**为 `webshell_sandbox_new.service`（`User=ease`、端口 8090、`WorkingDirectory=/opt/webshell_sandbox1`、`CONFIG_PATH=/opt/webshell_sandbox1/config.yaml`、`ExecStart=/opt/webshell_sandbox1/webshell`），具体以部署目录为准。

> 部署新二进制时需先 `systemctl stop webshell`，再替换 `/opt/webshell/webshell`，最后 `systemctl start webshell`（避免 `Text file busy`）。

> 早期曾用 `systemd-run --unit=webshell --collect` 创建 transient 单元（停止后自动清理、重启需重新创建），现已改用上面的持久化单元（`enabled` 开机自启）。

## 沙箱部署

启用 Linux 沙箱隔离时，除常规部署外还需：构建 3 个二进制（webshell / sandbox-init / userns-init）、构建 rootfs、配置 `config.yaml` 的 `sandbox` 块、配置 sudoers + cgroup + systemd 单元，最后启动验证。**详细部署见 DEPLOY.md，含 Ubuntu 与 RHEL（关闭 SELinux）**，此处仅列概要：

1. **构建 3 个二进制**：`go build` 主程序与 `sandbox-init`，`gcc -O2 -static` 编译 `userns-init`（见「编译构建」）。
2. **构建 rootfs**：在目标服务器上运行 `/opt/webshell_sandbox1/scripts/build_rootfs.sh`（需 root），构建到 `/opt/runner/rootfs/base`（约 500–600MB）。
3. **配置 config.yaml**：启用 `sandbox.enabled: true`，设置 `init_path`、`rootfs_path`、`network`、`bind_mounts`、`bind_hosts` 等（见「配置说明」）。
4. **sudoers + cgroup + systemd**：添加沙箱 sudoers 规则；预创建 `/sys/fs/cgroup/webshell_sandbox` 并启用 cpu/memory/pids 控制器；配置 `webshell-sandbox.service`（端口可配，示例 8090）。
5. **启动验证**：启动 systemd 服务，浏览器 AD 登录后，沙箱内 `id` 应显示真实用户名（uid/gid/补充组与宿主机一致），vim、opencode、NFS bind 路径可用。

> RHEL 差异：需关闭 SELinux、适配 `/usr/lib64` 库路径的 rootfs 脚本、firewalld 放行端口，详见 DEPLOY.md。

## opencode / codex 版本更新

采用**方案 B（bind 方式）**：`/share/apps/.opencode` 与 `/share/apps/.codex-standalone` 已通过 `sandbox.bind_mounts` bind 进沙箱，沙箱内直接读取宿主最新版本，**无需再拷贝进 rootfs**。更新版本只需替换宿主 `/share/apps` 下对应二进制/目录，**新开的沙箱会话即生效**（无需重启服务、无需重建 rootfs）。

```bash
# opencode：替换宿主二进制（新开沙箱会话生效）
cp 新版本opencode /share/apps/.opencode/bin/opencode
chmod 755 /share/apps/.opencode/bin/opencode

# codex：替换 /share/apps/.codex-standalone/ 下内容（codex 为启动脚本 + vendor/ 依赖目录）
cp -a 新版本codex/. /share/apps/.codex-standalone/
chmod 755 /share/apps/.codex-standalone/codex
```

验证（在沙箱会话内执行）：

```bash
/share/apps/.opencode/bin/opencode --version
/share/apps/.codex-standalone/codex --version
```

> **安全注意**：bind 后沙箱内（登录用户身份）对 `/share/apps/.opencode` 与 `/share/apps/.codex-standalone` 可写（未做只读 remount）。实际 codex/opencode 以只读方式执行，被篡改风险低；rootfs 中的旧副本保留作为 fallback 无害。

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
ease ALL=(root) NOPASSWD: /opt/webshell_sandbox1/sandbox-init
```

> - 沙箱二进制经 `sudo -n -u root /opt/webshell_sandbox1/sandbox-init`（**无 `-i`**）以 root 启动，内部完成 unshare/pivot_root 后降权到真实用户（含补充组）；
> - `/opt/webshell_sandbox1` 目录必须 **root:root 0755**，防止服务用户替换 `sandbox-init` 二进制提权 root；
> - 原 `ease ALL=(ALL,!root) NOPASSWD: ALL` 规则可保留（用于非沙箱会话及配置同步），与沙箱规则互不冲突；
> - cgroup 根目录（如 `/sys/fs/cgroup/webshell_sandbox`）需 root 预创建，并启用 cpu/memory/pids 控制器；systemd 开机自启见 DEPLOY.md。

### 2. 服务自身目录

| 路径 | 权限要求 | 说明 |
| ---- | -------- | ---- |
| 二进制 `/opt/webshell/webshell` | 服务用户可读、可执行 | `chown ease:ease` |
| 配置 `/opt/webshell/config.yaml` | 服务用户可读（含 AD 密码，建议 `0600`） | `chown ease:ease; chmod 600`（**属主必须为服务用户**，否则 systemd（`User=ease`）启动即报 `config error ... permission denied` 且 `Restart=on-failure` 无限重启） |
| 工作目录 `/opt/webshell` | 服务用户可读、可进入 | |

### 3. opencode / codex 相关资源（供服务用户读取）

| 路径 | 权限要求 |
| ---- | -------- |
| opencode 二进制 `/share/apps/.opencode/bin/opencode` | 所有用户可执行（含服务用户） |
| 配置源目录 `/share/apps/.opencode-config`（含 `skills/` 与 `opencode.jsonc`） | 服务用户可读（`sync_from` 指向） |
| codex 二进制 `/share/apps/.codex-standalone/codex` | 所有用户可执行（含服务用户） |
| 配置源目录 `/share/apps/.codex-config`（含 `config.toml`） | 服务用户可读（`sync_from` 指向） |

> 两源目录下的 `AGENTS.md`（源 `<syncFrom>/AGENTS.md`，每次启动缺失才补）同样需对服务用户可读（当前 root:root 644），否则 `cp` 会失败（同步失败仅 log 不阻塞，与现有 config.toml 同类部署约束）。`agent/`（opencode）与 `agents/`（codex）源目录（策略 A 整体复制）同理需对服务用户可读。

> `/share/apps/.opencode` 与 `/share/apps/.codex-standalone` 已通过 `sandbox.bind_mounts` bind 进沙箱（方案 B），沙箱内直接读取宿主版本；更新版本见「opencode / codex 版本更新」。

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
- `mode=codex` 且 codex 已启用 → 先同步配置，再以该用户启动 codex TUI；
- 其余情况（未传 / `mode=bash` / 对应代理未启用）→ 直接进入该用户的 bash。

**沙箱模式启动**：启用沙箱（`sandbox.enabled=true`）时，会话通过 `sudo -n -u root /opt/webshell_sandbox1/sandbox-init <args>`（无 `-i`）启动：root 阶段完成 enterCgroup → 预创建 rootfs 挂载点 → resolveSupplementaryGroups → exec `userns-init`（C，root 单线程）执行 `unshare(NS|PID|NET)` → `make-rprivate` → fork → exec `sandbox-init --child`（root）；子进程 `buildRootFS`（bind rootfs/home/NFS/dev/proc）→ `pivot_root` → rlimit(best-effort) → seccomp → `upLoopback` → `dropToUser`（setgroups 主组+补充组 → setgid → setuid）→ exec bash/opencode。沙箱内进程是**宿主真实用户身份**（含补充组，非 root、非 userns root），解决 NFS 因丢失补充组导致 Permission denied 的问题。非沙箱模式（`sandbox.enabled=false`）退回传统 `sudo -n -u <user> -i bash`。

**退出行为**：

- **bash 模式**：退出 bash 即结束会话，前端回到登录界面；
- **opencode 模式**：退出 opencode TUI 即完全结束会话（不退回 bash），前端回到登录界面；
- **codex 模式**：退出 codex TUI 即完全结束会话（不退回 bash），前端回到登录界面。

**刷新保持会话**：页面刷新或短暂断线时，WebSocket 连接断开但 pty 会话会保留在服务端会话仓库（`handler/session_hub.go`）中一段时间（默认 5 分钟），前端把会话标识存入 `sessionStorage`，刷新后自动通过 `/api/resume` 探测并重连到同一终端——正在运行的进程（vim、命令等）不丢失。关闭标签页后 `sessionStorage` 清除，会话超时后被巡检自动回收。

## WebSocket 协议

`/ws?token=<token>&mode=<bash|opencode|codex>` 升级后，消息为 JSON 信封（`protocol/message.go`）：

| 类型     | 方向         | 说明                                             |
| -------- | ------------ | ------------------------------------------------ |
| `input`  | 客户端→服务端 | 写入 pty 的输入字节（JSON 字符串）               |
| `resize` | 客户端→服务端 | 设置终端尺寸 `{cols, rows}`                       |
| `output` | 服务端→客户端 | pty 输出字节（base64 编码后放入字符串）          |
| `close`  | 服务端→客户端 | 会话结束/错误 `{reason}`                          |
| `session`| 服务端→客户端 | 会话标识 `{id}`（刷新后用于恢复同一终端）         |
| `auth-request` | 服务端→客户端 | opencode/codex 启动前请求补全缺失 api_key：`{envVars: string[], timeout}` |
| `auth-response` | 客户端→服务端 | 回传单个 api_key：`{envVar, apiKey}`（逐项发送）   |
| `auth-cancel`   | 客户端→服务端 | 用户取消补全流程                                  |

恢复会话时前端改为连接 `/ws?session=<id>&username=<user>`（无需一次性 token），服务端据此重新附着到同一 pty 会话。

## 测试

```bash
go test ./...
```

当前测试覆盖 `auth` 包（配置加载默认值、必填项校验、YAML 解析、codex env_key 提取）、`pty` 包（syncCodexCommand / syncConfigCommand 命令形态，含 AGENTS.md 与 agent/agents 目录每次启动补全片段断言）、`sandbox` 包（validate 对 codex 分支的校验）等。

## 安全说明

- `config.yaml` 含 AD manager 密码，建议权限设为 `0600`；
- 日志不会记录密码原文；含 `@` 的邮箱登录名统一记为 `<email>` 占位；
- 会话以目标用户运行（沙箱模式下为沙箱内真实用户身份，非沙箱模式为 `sudo -n -u <user> -i bash`），服务用户本身无该用户家目录访问权限时不受影响（工作目录设为 `/`，由 `sudo -i` / 沙箱切到用户家目录）；
- 会话标识（sessionID）为 32 字节高熵随机值，作为恢复会话的 bearer 凭证并绑定用户名；断线保留设 5 分钟超时，超时由巡检自动回收，避免会话无限存活。

**沙箱安全**（启用沙箱时）：

- `/opt/webshell_sandbox1` 目录属主必须 **root:root 0755**，防止服务用户替换 `sandbox-init` / `userns-init` 二进制提权 root；sudoers 仅放行精确路径 `sandbox-init`；
- rootfs 内无 setuid 二进制（如 `su` 已去除 setuid 位），沙箱内 `/etc` 只读（pivot_root + 只读 rootfs）；
- seccomp 拦截 mount/unshare/setns 及新版挂载 API 等（沙箱内不可再创建命名空间/挂载）；
- cgroup 限制 memory/cpu/pids 生效，防止资源滥用；
- `/proc` 以 `hidepid=2` 挂载 + PID namespace，沙箱内 `ps` 只见本会话进程；
- 沙箱内进程为宿主真实用户身份（非 root、非 userns root），文件归属与权限正确，且不越过用户自身权限。
