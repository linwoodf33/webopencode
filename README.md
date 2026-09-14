# Web Shell

一个基于 Web 的远程终端服务：用户通过浏览器登录（AD/LDAPS 强认证）后，可进入交互式终端会话。支持在登录后选择打开**普通 Bash 终端**或 **opencode TUI**，并可自动将共享的 opencode 配置同步到各用户的家目录。

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
- **以目标用户身份运行**：通过 `sudo -n -u <user> -i bash` 切换到登录用户，确保文件归属与权限正确。
- **终端字号调整**：终端工具条提供 `A-`/`A+` 按钮调整字号（8–32px，步进 2px），选择持久化到 `localStorage`，下次登录自动恢复。
- **终端主题切换**：工具条下拉框内置 8 个流行主题（VS Code Dark、Dracula、Monokai、Nord、Solarized Dark、One Dark、Tokyo Night、GitHub Light），实时切换并持久化，下次登录自动恢复。
- **刷新保持会话**：页面刷新或短暂断线后自动重连到同一终端，正在运行的进程不丢失（详见「会话模式与会话行为」）。

## 目录结构

```
.
├── main.go                  # 入口：配置加载、路由注册、静态资源托管、服务启动
├── static_embed.go          # //go:embed 内嵌 static/ 到二进制
├── config.example.yaml      # 配置示例（复制为 config.yaml 使用）
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

需要 Go 1.26.5 或更高版本。

```bash
go build -o webshell .
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
```

> 配置同步仅在用户首次初始化（`~/.config/opencode/skills` 不存在）时执行；一旦 `skills` 目录已存在，则跳过全部同步（opencode.jsonc 与 skills 均不覆盖），保留用户已有配置。`overwrite` 仅在首次初始化时生效。

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
- 会话通过 `sudo -n -u <user> -i bash` 以目标用户运行，服务用户本身无该用户家目录访问权限时不受影响（工作目录设为 `/`，由 `sudo -i` 切换到用户家目录）；
- 会话标识（sessionID）为 32 字节高熵随机值，作为恢复会话的 bearer 凭证并绑定用户名；断线保留设 5 分钟超时，超时由巡检自动回收，避免会话无限存活。
