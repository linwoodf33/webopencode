# WebShell Sandbox 部署指南（第三方服务器）

本文档描述如何将 **WebShell + Linux 沙箱隔离系统** 部署到一台新的第三方服务器。
系统基于 Go WebShell（AD 认证）+ 轻量级 Linux 沙箱（mount/PID/net namespace + pivot_root + cgroup + seccomp），
沙箱内以宿主真实用户身份运行（保留补充组），支持 NFS bind、/etc/hosts 映射、vim、opencode 等。

---

## 一、环境要求

| 项 | 要求 | 说明 |
|----|------|------|
| OS | Ubuntu 22.04 类（内核 5.15+） | 需支持 cgroup v2、user namespace、NFS |
| 内核 | 5.15+ | 已验证 5.15.0-185-generic |
| Go | 1.26.5+ | 编译 webshell / sandbox-init |
| gcc | 任意（Ubuntu 11.x 验证） | 编译 userns_init.c |
| systemd | 有 | 托管服务 |
| sudo | 1.9.x | sudoers 配置 |
| cgroup v2 | 已挂载 | 需 root 预创建 cgroup 根目录并启用控制器 |
| NFS 客户端 | 已装 | 若需要 bind NFS 挂载点 |
| AD/LDAPS | 可访问 | 认证依赖 |

---

## 二、服务器准备

### 1. 安装基础工具
```bash
apt-get update
apt-get install -y gcc make libc6-dev build-essential nfs-common sudo
```

### 2. 确认 cgroup v2
```bash
mount | grep cgroup2
# 应输出: cgroup2 on /sys/fs/cgroup type cgroup2 ...
```

### 3. 创建运行服务用户（示例 ease）
```bash
useradd -r -m -s /usr/sbin/nologin ease
```

---

## 三、准备源代码与构建

### 1. 拷贝源码
将项目源码（含 `sandbox/` 目录）放到构建服务器，例如 `/root/webshell`：
```bash
# 从开发机同步（示例，按实际情况）
scp -r webshell/ root@<server>:/root/webshell
```

### 2. 构建 Go 二进制
```bash
cd /root/webshell
go build -o /opt/webshell_sandbox/webshell .
go build -o /opt/webshell_sandbox/sandbox-init ./sandbox
```

### 3. 编译 C 辅助程序（userns-init）
```bash
gcc -O2 -static -o /opt/webshell_sandbox/userns-init /root/webshell/sandbox/userns_init.c
```

### 4. 创建部署目录并设置权限
```bash
mkdir -p /opt/webshell_sandbox
chown root:root /opt/webshell_sandbox && chmod 0755 /opt/webshell_sandbox
chown root:root /opt/webshell_sandbox/sandbox-init /opt/webshell_sandbox/userns-init
chmod 0755 /opt/webshell_sandbox/sandbox-init /opt/webshell_sandbox/userns-init
chown ease:ease /opt/webshell_sandbox/webshell
```

> **重要安全要求**：`/opt/webshell_sandbox` 目录属主必须是 `root:root` 且 `0755`，
> 否则服务账号 ease 可替换 `sandbox-init` 二进制提权 root。

---

## 四、构建沙箱 rootfs

### 1. 获取 rootfs 构建脚本
`build_rootfs.sh` 位于已部署服务器的 `/opt/webshell_sandbox/scripts/build_rootfs.sh`（不在 Go 源码目录中）。
从已部署服务器拷贝到新服务器：
```bash
mkdir -p /opt/webshell_sandbox/scripts
scp <source-server>:/opt/webshell_sandbox/scripts/build_rootfs.sh /opt/webshell_sandbox/scripts/
```
> 该脚本从**目标服务器自身**的 /usr/bin 等拷贝二进制与依赖到 rootfs，因此必须在目标服务器上运行（它构建的是目标服务器的 rootfs）。

### 2. 运行构建脚本（需 root）
```bash
chmod +x /opt/webshell_sandbox/scripts/build_rootfs.sh
/opt/webshell_sandbox/scripts/build_rootfs.sh
```
- 目标目录：`/opt/runner/rootfs/base`（约 500-600MB）
- 脚本会从宿主拷贝 bash/coreutils/gcc/python3/make/vim 及其动态依赖
- 构建后自动 chroot 测试 bash 可用

### 3. 可选：加入 opencode 二进制到 rootfs
若需要在沙箱内运行 opencode，需把 opencode 二进制及其依赖拷入 rootfs：
```bash
ROOT=/opt/runner/rootfs/base
mkdir -p $ROOT/share/apps/.opencode/bin
cp -a /share/apps/.opencode/bin/opencode $ROOT/share/apps/.opencode/bin/
# 递归拷贝 opencode 的动态库依赖（ldd 逐个拷贝到对应路径）
```

---

## 五、配置

### 1. 编写 config.yaml
```bash
cat > /opt/webshell_sandbox/config.yaml <<'EOF'
# Web Shell Sandbox 配置
ad:
  server: "LDAPS_SERVER:636"        # 替换为实际 AD/LDAPS 地址
  manager_dn: "MANAGER@DOMAIN"      # 替换
  manager_password: "CHANGE_ME"     # 替换（建议 0600 权限）
  search_dn: "DC=example,DC=com"    # 替换
  tls_insecure: true
  timeout: 5

require_password: true
token_ttl: 300
listen: "0.0.0.0:8090"              # 端口可改

opencode:
  enabled: true
  path: "/share/apps/.opencode/bin/opencode"
  sync_from: "/share/apps/opencode-config"
  overwrite: true

sandbox:
  enabled: true
  init_path: "/opt/webshell_sandbox/sandbox-init"
  rootfs_path: "/opt/runner/rootfs/base"
  memory_max: "1G"
  cpu_quota: "200000 100000"
  pids_max: 128
  tmp_size: "1G"
  network: "full"          # none|loopback|full
  seccomp: true
  nofile_limit: 4096
  core_dump: false
  max_fsize: "1G"
  proc_hidepid: true
  dev_list: []
  cgroup_root: "/sys/fs/cgroup/webshell_sandbox"
  bind_mounts:
    - "/easeshare/SH/Method2"          # NFS 挂载点（宿主机须已挂载）
    - "/share/apps/opencode-config"    # opencode 共享配置
  bind_hosts: true
EOF
chown ease:ease /opt/webshell_sandbox/config.yaml
chmod 600 /opt/webshell_sandbox/config.yaml
```

### 2. 配置说明（关键点）
- `bind_mounts`：宿主机**已挂载**的路径（含 NFS）bind 进沙箱。简单列表形式 `- "/path"`（source=target）。
- `network`：
  - `full`：共享宿主网络（NFS 访问、opencode 联网）
  - `none`/`loopback`：沙箱无外部网络（NFS 将不可访问）
- `sandbox.enabled=false`：退回传统 sudo 直连（不叠加沙箱隔离）

---

## 六、宿主机准备（NFS 与 opencode）

### 1. 挂载 NFS（按需）
```bash
# 挂载 NFS 到宿主路径（示例）
mount -t nfs ibio16:/data/public/Method2 /easeshare/SH/Method2
mount -t nfs easemgt:/data/apps /share/apps
# 如需开机自动挂载，写入 /etc/fstab
```

### 2. opencode 二进制与配置
- opencode 二进制：`/share/apps/.opencode/bin/opencode`（沙箱内需 rootfs 内可执行）
- opencode 共享配置：`/share/apps/opencode-config`（含 `opencode.jsonc` + `skills/`）

---

## 七、系统配置（sudoers / cgroup / systemd）

### 1. sudoers（关键）
```bash
cat > /etc/sudoers.d/webshell-sandbox <<'EOF'
# 允许服务用户 ease 仅以 root 运行 sandbox-init（精确路径，最短提权面）
ease ALL=(root) NOPASSWD: /opt/webshell_sandbox/sandbox-init
EOF
chmod 440 /etc/sudoers.d/webshell-sandbox
chown root:root /etc/sudoers.d/webshell-sandbox
visudo -c   # 校验语法
```

> **注意**：若现有 `/etc/sudoers.d/ease` 有 `ease ALL=(ALL,!root) NOPASSWD: ALL`（旧版 WebShell 用），
> 保留它（用于非沙箱会话）；新规则精确到 sandbox-init 路径，互不冲突。

### 2. cgroup 根目录
```bash
mkdir -p /sys/fs/cgroup/webshell_sandbox
# 启用控制器（cpu/memory/pids）
echo "+cpu +memory +pids" > /sys/fs/cgroup/webshell_sandbox/cgroup.subtree_control
cat /sys/fs/cgroup/webshell_sandbox/cgroup.subtree_control
# 应输出: cpu memory pids
```
> 需 root 执行；重启后需重新设置（见下文"开机自启配置"）。

### 3. systemd 单元
```bash
cat > /etc/systemd/system/webshell-sandbox.service <<'EOF'
[Unit]
Description=Web Shell Sandbox (port 8090)
After=network.target

[Service]
Type=simple
User=ease
WorkingDirectory=/opt/webshell_sandbox
Environment=CONFIG_PATH=/opt/webshell_sandbox/config.yaml
ExecStart=/opt/webshell_sandbox/webshell
Restart=on-failure
RestartSec=3
NoNewPrivileges=no
ProtectSystem=full
PrivateTmp=no

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable webshell-sandbox.service
```

---

## 八、启动与验证

### 1. 启动服务
```bash
systemctl start webshell-sandbox.service
systemctl status webshell-sandbox.service
ss -tlnp | grep 8090   # 确认监听
```

### 2. 功能验证
```bash
# 验证 sudo 链路（服务用户 ease）
sudo -n -u root /opt/webshell_sandbox/sandbox-init --uid 1000 --gid 1000 \
  --home /home/testuser --session test1 --program bash \
  --rootfs /opt/runner/rootfs/base --network full \
  --cgroup-root /sys/fs/cgroup/webshell_sandbox -- /bin/true
# 无报错即 sandbox-init 链路正常
```

浏览器访问 `http://<server>:8090`，AD 登录后：
- `id` 应显示真实用户名（uid/gid/补充组与宿主机一致）
- `vim` 可用
- `ls /share/apps/opencode-config/` 可见配置
- NFS bind 路径可见（若配置）

---

## 九、开机自启配置（cgroup 控制器）

systemd 服务启动后 cgroup 根目录可能消失（取决于 /sys/fs/cgroup 挂载）。若需开机自动创建并启用控制器：

```bash
cat > /etc/systemd/system/webshell-cgroup.service <<'EOF'
[Unit]
Description=Prepare cgroup root for webshell sandbox
Before=webshell-sandbox.service

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'mkdir -p /sys/fs/cgroup/webshell_sandbox && echo "+cpu +memory +pids" > /sys/fs/cgroup/webshell_sandbox/cgroup.subtree_control'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable webshell-cgroup.service
```

---

## 十、故障排查

| 现象 | 可能原因 | 解决 |
|------|---------|------|
| 会话立即结束 | rlimit 设置失败（旧版）| 确认 sandbox-init 为最新版（rlimit best-effort）|
| sudo 要求密码 | 使用 `sudo -u root -i` | 确保代码用 `sudo -u root`（无 `-i`）|
| NFS 不可见 | bind 参数未解析 | 确认 bool 参数用等号形式（`--core-dump=false`）；检查服务日志 |
| NFS Permission denied | 沙箱丢失补充组 | 确认方案 A（非 userns，保留补充组）已部署 |
| 中文乱码 | 缺 locale | rootfs 拷入 `/usr/lib/locale/locale-archive`，env 设 `LANG=en_US.utf8` |
| cgroup 目录累积 | 会话清理失败 | 确认最新 userns-init（cleaner 移到根 cgroup + rmdir）|
| 沙箱提权风险 | 目录属主错误 | `/opt/webshell_sandbox` 必须 root:root 0755 |

---

## 十一、安全加固清单

- [ ] `/opt/webshell_sandbox` 属主 `root:root`、`0755`
- [ ] `config.yaml` 属主 `ease`、`0600`（含 AD 密码）
- [ ] sudoers 仅放行精确路径 `sandbox-init`
- [ ] rootfs 内无 setuid 二进制（`su` 已去除 setuid 位）
- [ ] 沙箱内 `ps` 只见本会话（PID ns + hidepid）
- [ ] 沙箱内 `/etc` 只读（pivot_root + 只读 rootfs）
- [ ] seccomp 拦截 mount/unshare/setns/新版挂载 API 等
- [ ] cgroup 限制 memory/cpu/pids 生效

---

## 十二、RHEL 系统部署差异（Red Hat Enterprise Linux 8/9、Rocky、AlmaLinux、CentOS Stream）

> 以下仅列出与 Ubuntu 流程的**差异点**；未提及的步骤（二进制构建、config.yaml、systemd、NFS bind、
> 故障排查、安全清单）与上文完全一致。

### 12.1 环境要求差异
| 项 | Ubuntu | RHEL 8/9 |
|----|--------|----------|
| 包管理 | `apt` | `dnf`（RHEL 8+）/ `yum` |
| 库目录 | `/usr/lib/x86_64-linux-gnu` | `/usr/lib64`（注意 rootfs 脚本差异）|
| SELinux | 无（默认） | **默认 enforcing，需处理** |
| cgroup v2 | 需确认 | RHEL 8.3+ / 9 默认 cgroup v2 |
| firewall | ufw | firewalld |

### 12.2 安装基础工具（RHEL）
```bash
dnf install -y gcc gcc-c++ make glibc-devel nfs-utils sudo vim-enhanced
# 可选：curl wget tar gzip
```

### 12.3 创建服务用户
```bash
useradd -r -m -s /usr/sbin/nologin ease
```

### 12.4 构建 Go 二进制
```bash
# 需要 Go 1.26.5+（RHEL 官方源通常较旧，建议用官方 Go 二进制）
# 下载 Go（示例 1.26.5，按实际版本替换）：
cd /tmp && curl -LO https://go.dev/dl/go1.26.5.linux-amd64.tar.gz
tar -C /usr/local -xzf go1.26.5.linux-amd64.tar.gz
export PATH=/usr/local/go/bin:$PATH
go version   # 确认

cd /root/webshell
go build -o /opt/webshell_sandbox/webshell .
go build -o /opt/webshell_sandbox/sandbox-init ./sandbox
gcc -O2 -static -o /opt/webshell_sandbox/userns-init sandbox/userns_init.c
chown root:root /opt/webshell_sandbox && chmod 0755 /opt/webshell_sandbox
chown root:root /opt/webshell_sandbox/sandbox-init /opt/webshell_sandbox/userns-init
chmod 0755 /opt/webshell_sandbox/sandbox-init /opt/webshell_sandbox/userns-init
chown ease:ease /opt/webshell_sandbox/webshell
```

### 12.5 rootfs 构建（RHEL 特有）

**关键差异**：`build_rootfs.sh` 依赖宿主的库目录结构。RHEL 的动态库在 `/usr/lib64`（而非
Ubuntu 的 `/usr/lib/x86_64-linux-gnu`），且 `ldd` 输出路径、`ld-linux-x86-64.so.2` 位置不同。
直接跑 Ubuntu 版脚本可能在 chroot 测试时失败（bash "No such file or directory"）。

**在 RHEL 上调整脚本要点**（修改 `build_rootfs.sh`）：
1. 目录结构新增 `/usr/lib64`：
   ```bash
   mkdir -p "$ROOT"/{usr/lib64,lib64}
   ```
2. 依赖库路径：RHEL 的库位于 `/usr/lib64/`，`copy_dep` 的目标路径保持宿主绝对路径
   （`${lib#/}`），符号链接可正确解析——脚本无需大改，只要**能正确处理 `/usr/lib64`**。
3. 强制链接器：RHEL 的 `ld-linux-x86-64.so.2` 位于 `/usr/lib64/ld-linux-x86-64.so.2`，
   脚本末尾的链接器拷贝段需把 `/usr/lib64` 纳入搜索：
   ```bash
   for cand in /lib64/ld-linux-x86-64.so.2 /usr/lib64/ld-linux-x86-64.so.2; do
     [ -e "$cand" ] && [ ! -e "$ROOT${cand#/}" ] && { mkdir -p "$(dirname "$ROOT${cand#/}")"; cp -a "$cand" "$ROOT${cand#/}"; }
   done
   ```
4. `/etc/passwd`、`/etc/group`、locale-archive 等基础文件照常写入；RHEL 的 locale-archive
   位于 `/usr/lib/locale/locale-archive`（与 Ubuntu 相同路径），拷贝即可支持中文：
   ```bash
   mkdir -p "$ROOT/usr/lib/locale"
   cp -a /usr/lib/locale/locale-archive "$ROOT/usr/lib/locale/" 2>/dev/null
   ```
5. 运行后必须通过 chroot 测试：`chroot "$ROOT" /bin/bash -c 'echo OK; id; ls /'`，
   若报 `No such file or directory`，说明链接器/库未拷全，按上文定位修复。

### 12.6 SELinux 处理（RHEL 部署方式：**关闭 SELinux**）

RHEL 默认 SELinux **enforcing**，会阻止沙箱的 `unshare(CLONE_NEWNS/NEWPID/NEWNET)`、
`pivot_root`、`mount` 等操作（audit 日志出现 `avc: denied { ... }`），导致会话无法启动。

**本项目的 RHEL 部署方式为关闭 SELinux**：

```bash
# 临时关闭（立即生效，重启失效）：
setenforce 0

# 持久关闭（写入配置文件，重启后仍生效）：
sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config
# 或设为 permissive（记录但不阻止，便于排查）：
# sed -i 's/^SELINUX=.*/SELINUX=permissive/' /etc/selinux/config
```

> - `disabled`：完全关闭（需重启生效，若当前为 enforcing 先 `setenforce 0`）；
> - `permissive`：只记录违规不阻止（生产不建议长期使用）；
> - 部署流程建议：先 `setenforce 0` 验证沙箱可用，再写入 `/etc/selinux/config` 持久化。

**验证**：
```bash
getenforce            # 应显示 Permissive 或 Disabled
ausearch -m avc -ts recent | tail   # 查看是否有 SELinux 拒绝日志（应无沙箱相关）
```

### 12.7 sudoers（RHEL）
与 Ubuntu 相同，但 RHEL sudoers 需 `visudo -c` 校验；文件权限 `0440`：
```bash
echo 'ease ALL=(root) NOPASSWD: /opt/webshell_sandbox/sandbox-init' > /etc/sudoers.d/webshell-sandbox
chmod 440 /etc/sudoers.d/webshell-sandbox
visudo -c
```

### 12.8 cgroup 根（RHEL）
RHEL 8/9 默认 cgroup v2（`/sys/fs/cgroup`）。与 Ubuntu 相同：
```bash
mkdir -p /sys/fs/cgroup/webshell_sandbox
echo "+cpu +memory +pids" > /sys/fs/cgroup/webshell_sandbox/cgroup.subtree_control
cat /sys/fs/cgroup/webshell_sandbox/cgroup.subtree_control
# 应输出: cpu memory pids
```

### 12.9 firewalld 放行端口
RHEL 默认启用 firewalld，需放行服务端口：
```bash
firewall-cmd --permanent --add-port=8090/tcp
firewall-cmd --reload
```

### 12.10 systemd 单元（RHEL）
与 Ubuntu 完全一致（见第七节），`User=ease`。启动：
```bash
systemctl daemon-reload
systemctl enable --now webshell-sandbox.service
ss -tlnp | grep 8090
```

### 12.11 RHEL 特有故障排查
| 现象 | 原因 | 解决 |
|------|------|------|
| 会话无法启动，journalctl 有 `avc: denied` | SELinux 阻止 unshare/pivot_root/mount | 按 12.6 **关闭 SELinux**（`setenforce 0` + 写配置）|
| 沙箱内 bash `No such file or directory` | rootfs 链接器/库未拷全（RHEL lib64 路径）| 见 12.5（补 `/usr/lib64`）|
| `go: command not found` | Go 未安装或 PATH 未设置 | 用官方 Go 二进制并 export PATH |
| NFS 不可见 | bind 参数未解析 或 SELinux 拦 bind mount | 检查 bool 参数等号形式 + 确认已关闭 SELinux |

---

> **RHEL 部署核心差异总结**：① 包管理 dnf；② 库路径 `/usr/lib64`（rootfs 脚本适配）；
> ③ **关闭 SELinux**（本项目 RHEL 部署方式）；④ firewalld 放行端口。
> 其余流程（构建、config、sudoers、systemd、NFS、验证、安全清单）与 Ubuntu 一致。
