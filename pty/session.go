package pty

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"webshell/auth"
)

// Session 封装一个 pty + bash 进程会话。
type Session struct {
	mu       sync.Mutex
	once     sync.Once
	master   *os.File
	cmd      *exec.Cmd
	closed   bool
	workDir  string
	username string
	// authEnvFile 沙箱模式 opencode 会话写入的 .auth.env 路径（api_key 环境文件），
	// 非空时 Close 时以目标用户身份删除（清理敏感文件）。
	authEnvFile string
}

// NewSession 创建一个新的 pty 会话，并在其中启动 bash。
// workDir 为 bash 的工作目录。
func NewSession(workDir string) (*Session, error) {
	if workDir == "" {
		workDir = "/"
	}

	// 使用 exec.LookPath 找到 bash 可执行文件。
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return nil, errors.New("cannot find bash: " + err.Error())
	}

	cmd := exec.Command(bashPath, "-i")
	cmd.Dir = workDir
	// 设置更友好的环境。
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"PS1=\\u@\\h:\\w\\$ ",
	)

	// 手工创建 pty 并启动 bash，stderr 直接指向 slave（与 creack/pty.Start 一致），
	// 保证交互式命令的 stderr 能进终端，且不会破坏对 pty master 的读取。
	master, err := startWithPty(cmd)
	if err != nil {
		return nil, errors.New("pty.Start failed: " + err.Error())
	}

	return &Session{
		master:  master,
		cmd:     cmd,
		workDir: workDir,
	}, nil
}

// startWithPty 创建一个 pty，将 slave 作为命令的 stdin/stdout/stderr
// （完全复刻 creack/pty 的 Start/StartWithAttrs 语义），并返回 pty 的 master。
//
// 注意：slave 会在 cmd.Start 成功返回后立即关闭（如同 creack/pty 的
// StartWithAttrs 用 `defer tty.Close()`），因为 stdin/stdout/stderr 均直接
// 指向 slave fd，exec 包不需要 pipe + goroutine 复制，本端不再持有其引用。
func startWithPty(cmd *exec.Cmd) (*os.File, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	// slave 在 Start 后即可安全关闭（exec 已持有其 fd 的副本用于子进程）。
	defer func() { _ = slave.Close() }() // Best effort.

	// stdin/stdout/stderr 全部直接指向 slave，保证交互式命令的 stderr 进终端，
	// 且保持对 pty master 的读取不被破坏（stderr 若为 io.MultiWriter 会改用
	// pipe + goroutine 复制，实测会导致 master 读不到任何输出）。
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	// 模拟 pty.Start 的会话/控制终端设置。
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true

	if err := cmd.Start(); err != nil {
		_ = master.Close() // Best effort.
		return nil, err
	}
	return master, nil
}

// NewSessionForUser 以指定 Linux 用户启动一个 pty + bash 会话。
//
// 通过 `sudo -n -u <username> -i bash` 切换到目标用户（-n 非交互、-i 登录 shell），
// sudo 使用绝对路径 /usr/bin/sudo。会话工作目录设为该用户的 HomeDir。
//
// username 与 homeDir 必须由调用方完成系统存在性校验；若为空返回错误。
func NewSessionForUser(username, homeDir string) (*Session, error) {
	return NewSessionForUserMode(username, homeDir, nil, nil, nil, "", nil)
}

// NewSessionForUserMode 以指定 Linux 用户启动一个 pty 会话。
//
// opencode / codex 均为 nil 或各自 Enabled == false 时，行为与 NewSessionForUser
// 一致（进入 bash，逃生通道为 bash 本身）。判定优先级：opencode > codex > bash
// （实际由 ws.go 保证只传其一）。
//
// opencode.Enabled == true 时，通过
//
//	/usr/bin/sudo -n -E -u <username> -i bash -c 'cd <homeDir> && <opencodePath>'
//
// 启动 opencode TUI（以登录用户身份运行、工作目录为用户家目录）。外层仍是 bash，
// 但 opencode 退出后 bash -c 即结束，从而完全退出登录会话（不退回 bash）。
//
// codex.Enabled == true 时，通过
//
//	/usr/bin/sudo -n -E -u <username> -i bash -c 'cd <homeDir> && <codexPath>'
//
// 启动 codex TUI（与 opencode 同形态）。
//
// sandboxCfg 非 nil 且 Enabled == true 时，改为经 setuid-root 的 sandbox-init
// 启动 Linux 用户命名空间沙箱（见下）。sandboxCfg 为 nil / 未启用时，保持上述
// 传统 sudo 路径，零回归。sessionID 用于沙箱 cgroup 命名，仅在沙箱模式下使用。
//
// username 与 homeDir 必须由调用方完成系统存在性校验；若为空返回错误。
// authEnv 为代理模式启动前补齐的 {环境变量: api_key} map，注入到代理进程环境；
// 非代理模式 / 无补齐项时传 nil 或空 map。
func NewSessionForUserMode(username, homeDir string, opencode *auth.OpencodeConfig, codex *auth.CodexConfig, sandboxCfg *auth.SandboxConfig, sessionID string, authEnv map[string]string) (*Session, error) {
	if username == "" {
		return nil, errors.New("empty username")
	}
	if homeDir == "" {
		homeDir = "/"
	}

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return nil, errors.New("cannot find sudo: " + err.Error())
	}

	// 沙箱模式：经 sandbox-init 启动用户命名空间沙箱。
	if sandboxCfg != nil && sandboxCfg.Enabled {
		return newSandboxSession(username, homeDir, opencode, codex, sandboxCfg, sessionID, sudoPath, authEnv)
	}

	// 构造 sudo 命令参数。
	// - bash 模式：`sudo -n -u <user> -i bash`
	// - opencode 模式：`sudo -n -E -u <user> -i bash -c 'cd <homeDir> && <opencodePath>'`
	// - codex 模式：`sudo -n -E -u <user> -i bash -c 'cd <homeDir> && <codexPath>'`
	var cmd *exec.Cmd
	switch {
	case opencode != nil && opencode.Enabled:
		if opencode.Path == "" {
			return nil, errors.New("opencode enabled but path is empty")
		}
		// opencode 模式：`sudo -n -E -u <user> -i bash -c 'cd <homeDir> && <opencodePath>'`
		//   - cd <homeDir> && 保证 cwd（homeDir 来自系统 user.Lookup，无注入风险）
		//   - opencode 作为前台子进程运行；opencode 退出后 bash -c 立即结束，
		//     从而完全退出登录会话（不再退回 bash）。
		//   - -E 保留调用者环境：authEnv 里的 api_key 通过 cmd.Env 传入并透传给
		//     opencode 进程（若部署侧 sudoers 对 -E 有 env_keep 限制，见 DEPLOY.md）。
		command := "cd " + homeDir + " && " + opencode.Path
		cmd = exec.Command(sudoPath, "-n", "-E", "-u", username, "-i", "bash", "-c", command)
	case codex != nil && codex.Enabled:
		if codex.Path == "" {
			return nil, errors.New("codex enabled but path is empty")
		}
		// codex 模式：与 opencode 同形态，`sudo -n -E -u <user> -i bash -c 'cd <homeDir> && <codexPath>'`
		//   - codex 作为前台子进程运行；codex 退出后 bash -c 立即结束，完全退出会话。
		//   - -E 保留调用者环境：authEnv 里的 api_key 经 env_key 被 codex 读取。
		command := "cd " + homeDir + " && " + codex.Path
		cmd = exec.Command(sudoPath, "-n", "-E", "-u", username, "-i", "bash", "-c", command)
	default:
		// sudo -n -u <username> -i bash
		cmd = exec.Command(sudoPath, "-n", "-u", username, "-i", "bash")
	}

	// cmd.Dir 是 sudo 进程 fork/exec 前的初始工作目录，由运行服务的用户（ease）
	// 负责 chdir。它绝不能设为目标用户家目录（如 /home/ubixz009）：家目录通常
	// 权限为 750（drwxr-x---），运行服务的用户无权进入，会导致 sudo fork/exec
	// 时因无法 chdir 而报 "fork/exec /usr/bin/sudo: permission denied"。
	// 这里使用 "/"（对服务用户总是可进入），真正切换到目标用户家目录的工作
	// 由 `sudo -i` 在登录 shell 启动时完成。
	cmd.Dir = "/"

	// 为会话设置目标用户的环境。`sudo -i` 会重建目标用户环境，这里只需保留
	// 终端相关变量；bash 模式额外保留 PS1 作为友好提示符（可选）。
	env := []string{
		"HOME=" + homeDir,
		"USER=" + username,
		"USERNAME=" + username,
		"LOGNAME=" + username,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	}
	if (opencode == nil || !opencode.Enabled) && (codex == nil || !codex.Enabled) {
		env = append(env, "PS1=\\u@\\h:\\w\\$ ")
	}
	// 注入代理模式启动前补齐的 api_key 环境变量（sudo -E 透传给目标用户进程）。
	for k, v := range authEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = append(os.Environ(), env...)

	// 启动 sudo 会话。stderr 直接进入 pty slave：sudo 启动失败（如 sudoers 未配置、
	// 未知用户）的错误会经 pty 输出到 master，由 handler 的 writeLoop 通过
	// output 消息推给前端（用户可感知），随后 bash 未启动、writeLoop 读到 EOF
	// 会 sendClose("shell exited") 结束会话。
	master, err := startWithPty(cmd)
	if err != nil {
		return nil, errors.New("sudo/pty.Start failed: " + err.Error())
	}

	return &Session{
		master:   master,
		cmd:      cmd,
		workDir:  homeDir,
		username: username,
	}, nil
}

// newSandboxSession 经 setuid-root 的 sandbox-init 启动 Linux 用户命名空间沙箱。
//
// 命令形态（服务进程 ease 仍以普通用户运行，经 `sudo -n -u root` 短暂提权
// 启动 sandbox-init；sandbox-init 内部解析后 setuid 回真实 UID 并降权）：
//
//	/usr/bin/sudo -n -u root <InitPath> \
//	    --uid <uid> --gid <gid> --home <homeDir> \
//	    --session <sessionID> --program <bash|opencode|codex> \
//	    [--opencode-path <path>] [--codex-path <path>] --rootfs <rootfs> \
//	    --network <network> --memory <memory> --cpu <cpu> --pids <pids> \
//	    --tmp-size <tmp> --nofile <nofile> --core-dump <bool> --max-fsize <fsize> \
//	    --proc-hidepid <bool> --cgroup-root <cgroupRoot>
//
// sessionID 与 cgroup 会话名一致（由调用方 ws.go 生成并传入），保证可追溯。
// username/homeDir 必须已完成系统存在性校验。
func newSandboxSession(username, homeDir string, opencode *auth.OpencodeConfig, codex *auth.CodexConfig, sandboxCfg *auth.SandboxConfig, sessionID string, sudoPath string, authEnv map[string]string) (*Session, error) {
	if sandboxCfg == nil {
		return nil, errors.New("sandbox config is nil")
	}
	if !sandboxCfg.Enabled {
		return nil, errors.New("sandbox not enabled")
	}
	if sessionID == "" {
		return nil, errors.New("empty sessionID")
	}

	// 取真实用户数字 UID/GID。
	u, err := user.Lookup(username)
	if err != nil {
		return nil, errors.New("user lookup failed: " + err.Error())
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, errors.New("invalid uid: " + err.Error())
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, errors.New("invalid gid: " + err.Error())
	}

	program := "bash"
	args := []string{
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--home", homeDir,
		"--session", sessionID,
	}
	switch {
	case opencode != nil && opencode.Enabled:
		program = "opencode"
		args = append(args, "--program", "opencode", "--opencode-path", opencode.Path)
	case codex != nil && codex.Enabled:
		program = "codex"
		args = append(args, "--program", "codex", "--codex-path", codex.Path)
	default:
		args = append(args, "--program", "bash")
	}
	args = append(args,
		"--rootfs", sandboxCfg.RootfsPath,
		"--network", sandboxCfg.NetworkMode(),
		"--tmp-size", sandboxCfg.TmpSize,
		"--nofile", strconv.Itoa(sandboxCfg.NoFileLimit),
		"--core-dump="+strconv.FormatBool(sandboxCfg.CoreDump),
		"--max-fsize", sandboxCfg.MaxFsize,
		"--proc-hidepid="+strconv.FormatBool(sandboxCfg.ProcHidepidEnabled()),
		"--cgroup-root", sandboxCfg.CgroupRoot,
	)
	if sandboxCfg.MemoryMax != "" {
		args = append(args, "--memory", sandboxCfg.MemoryMax)
	}
	if sandboxCfg.CPUQuota != "" {
		args = append(args, "--cpu", sandboxCfg.CPUQuota)
	}
	if sandboxCfg.PidsMax > 0 {
		args = append(args, "--pids", strconv.Itoa(sandboxCfg.PidsMax))
	}
	if !sandboxCfg.SeccompEnabled() {
		args = append(args, "--seccomp=false")
	} else {
		args = append(args, "--seccomp=true")
	}
	if sandboxCfg.BindHosts {
		args = append(args, "--bind-hosts")
	}
	if sandboxCfg.BindResolv {
		args = append(args, "--bind-resolv")
	}
	if sandboxCfg.UsernsInitPath != "" {
		args = append(args, "--userns-init-path", sandboxCfg.UsernsInitPath)
	}
	if sandboxCfg.HTTPProxy != "" {
		args = append(args, "--http-proxy", sandboxCfg.HTTPProxy)
	}
	if sandboxCfg.HTTPSProxy != "" {
		args = append(args, "--https-proxy", sandboxCfg.HTTPSProxy)
	}
	if sandboxCfg.NoProxy != "" {
		args = append(args, "--no-proxy", sandboxCfg.NoProxy)
	}
	for _, m := range sandboxCfg.BindMounts {
		args = append(args, "--bind-mount", m.Source, "--bind-mount-target", m.Target)
	}

	// opencode 模式下若有补齐的 api_key：以目标用户身份写入 .auth.env 文件
	// （内容 export KEY='val'，0600），并把路径透传给 sandbox-init 注入环境。
	// 沙箱内家目录被 bind 到与宿主相同相对路径，childInit 降权后（目标用户身份）
	// 可直接读取该文件。api_key 不进 sandbox-init 命令行参数/进程列表。
	authEnvFile := ""
	if len(authEnv) > 0 {
		f, werr := WriteAuthEnvFile(username, homeDir, authEnv)
		if werr != nil {
			return nil, werr
		}
		authEnvFile = f
		args = append(args, "--auth-env-file", authEnvFile)
	}

	// sudo -n -u root <InitPath> ...
	cmd := exec.Command(sudoPath, append([]string{"-n", "-u", "root", sandboxCfg.InitPath}, args...)...)
	cmd.Dir = "/"

	env := []string{
		"HOME=" + homeDir,
		"USER=" + username,
		"USERNAME=" + username,
		"LOGNAME=" + username,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	}
	if program == "bash" {
		env = append(env, "PS1=\\u@\\h:\\w\\$ ")
	}
	cmd.Env = append(os.Environ(), env...)

	master, err := startWithPty(cmd)
	if err != nil {
		// 启动失败：清理已写入的 .auth.env（避免残留敏感文件）。
		if authEnvFile != "" {
			_ = RemoveAuthEnvFile(username, homeDir)
		}
		return nil, errors.New("sandbox/sudo/pty.Start failed: " + err.Error())
	}

	return &Session{
		master:      master,
		cmd:         cmd,
		workDir:     homeDir,
		username:    username,
		authEnvFile: authEnvFile,
	}, nil
}

// agentsSyncSnippet 返回「补全全局 AGENTS.md」的 shell 片段。
// 语义：独立于整体同步跳过标记，每次启动都检查；目标缺失且源存在才复制；
// 已存在不覆盖；源缺失静默跳过；与 overwrite 无关。
func agentsSyncSnippet(dest, syncFrom string) string {
	return "if [ ! -f " + dest + "/AGENTS.md ] && [ -f " + syncFrom + "/AGENTS.md ]; then mkdir -p " + dest + " && cp " + syncFrom + "/AGENTS.md " + dest + "/AGENTS.md; fi"
}

// agentDirSyncSnippet 返回「补全 agent/agents 目录」的 shell 片段。
// 目录名由 agentDir 传入（opencode 用 "agent"，codex 用 "agents"）。
// 语义（策略 A）：源目录存在且非空、目标目录不存在才整体复制；目标已有则跳过
// （保留用户自定义）；源目录为空时不创建目标空目录；独立于整体跳过标记；与 overwrite 无关。
func agentDirSyncSnippet(dest, syncFrom, agentDir string) string {
	return "if [ -d " + syncFrom + "/" + agentDir + " ] && [ ! -d " + dest + "/" + agentDir + " ] && [ -n \"$(ls -A " + syncFrom + "/" + agentDir + " 2>/dev/null)\" ]; then mkdir -p " + dest + " && cp -r " + syncFrom + "/" + agentDir + " " + dest + "/" + agentDir + "; fi"
}

// 通过 sudo -u <username> 执行，使复制出的文件自然归目标用户所有（避开服务用户
// ease 无 chown 权限的问题）。homeDir/syncFrom/opencodePath 均来自系统 user.Lookup
// 与配置，无注入风险。
//
// 当用户家目录已存在 .config/opencode/skills 目录时，跳过全部同步（opencode.jsonc
// 与 skills 均不复制，视为已初始化），避免覆盖用户已有配置。
//
// 在整体同步 if...fi 之后追加 AGENTS.md 与 agent 目录补全片段
// （agentsSyncSnippet / agentDirSyncSnippet）：
//   - AGENTS.md：每次启动都检查 ~/.config/opencode/AGENTS.md，缺失且源存在才从
//     <syncFrom>/AGENTS.md 复制；已存在不覆盖（保留用户编辑）；源缺失静默跳过。
//   - agent 目录（策略 A）：源 <syncFrom>/agent 存在且非空、目标
//     ~/.config/opencode/agent 不存在才整体复制；目标已有则跳过（保留用户自定义 agent）；
//     源目录为空时不创建目标空目录。
//     两者均独立于整体跳过标记（即使 skills 已存在导致整体同步跳过，仍照常检查），
//     且与 overwrite 无关（两分支片段一致）。
//
// 注意：返回的命令必须保持**完全单行**（无任何 \n），多个命令之间以分号 ; 分隔。
// 原因：命令经 `sudo -n -u <user> -i bash -c '<cmd>'` 执行时换行会被吞掉
// （fi\nif 粘连成 fiif 导致 bash 语法错误），只能用分号分隔保证语法合法。
func syncConfigCommand(username, homeDir, syncFrom string, overwrite bool) string {
	dest := homeDir + "/.config/opencode"
	if overwrite {
		// 全量覆盖：skills 目录整体复制进 dest，opencode.jsonc 直接覆盖。
		// 但若用户已有 skills 目录则整体跳过（已初始化）。
		return "if [ ! -d " + dest + "/skills ]; then mkdir -p " + dest + " && cp -r " + syncFrom + "/skills " + dest + "/ && cp " + syncFrom + "/opencode.jsonc " + dest + "/; fi; " + agentsSyncSnippet(dest, syncFrom) + "; " + agentDirSyncSnippet(dest, syncFrom, "agent")
	}
	// overwrite=false：opencode.jsonc 仅在目标不存在时复制，skills 仍覆盖。
	// 但若用户已有 skills 目录则整体跳过（已初始化）。
	return "if [ ! -d " + dest + "/skills ]; then mkdir -p " + dest + " && cp -r " + syncFrom + "/skills " + dest + "/ && ( [ -f " + dest + "/opencode.jsonc ] || cp " + syncFrom + "/opencode.jsonc " + dest + "/ ); fi; " + agentsSyncSnippet(dest, syncFrom) + "; " + agentDirSyncSnippet(dest, syncFrom, "agent")
}

// SyncOpencodeConfig 以目标用户身份，将共享 opencode 配置源同步到用户的
// ~/.config/opencode（含 skills/ 与 opencode.jsonc）。
//
// 通过 `sudo -n -u <username> -i bash -c '<cmd>'` 执行复制，使文件归目标用户所有。
// 同步失败返回 error（不阻塞登录，由调用方决定仅 log）。
//
// timeout 为整体执行超时；username/homeDir 必须已完成系统存在性校验。
func SyncOpencodeConfig(username, homeDir string, opencode *auth.OpencodeConfig, timeout time.Duration) error {
	if opencode == nil || !opencode.Enabled {
		return nil
	}
	if username == "" || homeDir == "" {
		return errors.New("empty username or homeDir")
	}
	if opencode.SyncFrom == "" {
		return errors.New("opencode sync_from is empty")
	}

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("cannot find sudo: " + err.Error())
	}

	cmdStr := syncConfigCommand(username, homeDir, opencode.SyncFrom, opencode.OverwriteEnabled())
	cmd := exec.Command(sudoPath, "-n", "-u", username, "-i", "bash", "-c", cmdStr)
	cmd.Dir = "/"
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sync opencode config failed: %v: %s", err, string(out))
	}
	return nil
}

// syncCodexCommand 构造「同步 codex 共享配置到用户 ~/.codex」的 bash 命令。
//
// 采用**白名单同步**（与 opencode 只同步 jsonc+skills 的模式对齐）：只复制
// config.toml 与 skills/ 两个配置项，绝不同步任何运行态/敏感文件
// （*.sqlite、*.jsonl、*.db*、日志、会话、installation_id、version.json、
// shell_snapshots/、thread-writer-locks/、tmp/ 等均不复制）。
//
// 命令形态（经 sudo -i 以目标用户身份执行，使复制出的文件归目标用户所有；
// homeDir/syncFrom 来自系统 user.Lookup 与配置，无注入风险）：
//
//	if [ ! -f <dest>/config.toml ]; then mkdir -p <dest> && cp <syncFrom>/config.toml <dest>/config.toml && if [ -d <syncFrom>/skills ]; then cp -r <syncFrom>/skills <dest>/skills; fi; fi; <AGENTS.md snippet>; <agents dir snippet>
//
// 注意：返回的命令必须保持**完全单行**（无任何 \n），多个命令之间以分号 ; 分隔。
// 原因：命令经 `sudo -n -u <user> -i bash -c '<cmd>'` 执行时换行会被吞掉
// （fi\nif 粘连成 fiif 导致 bash 语法错误），只能用分号分隔保证语法合法。
//   - 幂等标记：dest 为 <homeDir>/.codex，~/.codex/config.toml 存在即整体跳过
//     （保留用户已有 config.toml 与配置）。
//   - 白名单而非黑名单：共享源目录 /share/apps/.codex-config/ 实际混入了大量
//     运行态/敏感文件（goals_*.sqlite、logs_*.sqlite、memories_*.sqlite、
//     queue_*.sqlite、state_*.sqlite、thread_history_*.sqlite（含 -shm/-wal）、
//     history.jsonl、session_index.jsonl、installation_id、.sandbox_migration、
//     version.json、sessions/、shell_snapshots/、thread-writer-locks/、.tmp/、
//     tmp/ 等），黑名单无法覆盖，因此只同步 config.toml 与 skills/。
//   - 源 config.toml 需对目标用户可读（当前为 root 600，目标用户读不了会导致
//     cp 失败、同步失败仅 log 不阻塞）。属部署侧问题：部署阶段需 chmod 644，
//     代码层面无需处理。
//   - overwrite 语义：codex 模式下以 config.toml 存在性标记为主，overwrite=true/false
//     命令形态等价（config.toml 不存在时无用户配置可保护），字段仅为与 opencode
//     对称保留。
//   - AGENTS.md 补全：在整体 if...fi 之后追加 agentsSyncSnippet，每次启动都检查
//     ~/.codex/AGENTS.md，缺失且源存在才从 <syncFrom>/AGENTS.md 复制；已存在不覆盖
//     （保留用户编辑）；源缺失静默跳过。独立于整体跳过标记（即使 config.toml 已存在
//     导致整体同步跳过，AGENTS.md 仍照常检查），且与 overwrite 无关。
//   - agents 目录补全（策略 A）：在整体 if...fi 之后追加 agentDirSyncSnippet
//     （agentDir 为 "agents"），源 <syncFrom>/agents 存在且非空、目标 ~/.codex/agents
//     不存在才整体复制；目标已有则跳过（保留用户自定义 agent）；源目录为空时不创建
//     目标空目录。独立于整体跳过标记，且与 overwrite 无关。
func syncCodexCommand(username, homeDir, syncFrom string, overwrite bool) string {
	_ = overwrite // codex 模式下以 config.toml 存在性标记为主，两取值命令形态等价
	dest := homeDir + "/.codex"
	return "if [ ! -f " + dest + "/config.toml ]; then mkdir -p " + dest + " && cp " + syncFrom + "/config.toml " + dest + "/config.toml && if [ -d " + syncFrom + "/skills ]; then cp -r " + syncFrom + "/skills " + dest + "/skills; fi; fi; " + agentsSyncSnippet(dest, syncFrom) + "; " + agentDirSyncSnippet(dest, syncFrom, "agents")
}

// SyncCodexConfig 以目标用户身份，将共享 codex 配置源同步到用户的 ~/.codex
// （含 config.toml，注意不是 ~/.config/codex）。
//
// 通过 `sudo -n -u <username> -i bash -c '<cmd>'` 执行复制，使文件归目标用户所有。
// 同步失败返回 error（不阻塞登录，由调用方决定仅 log）。
//
// timeout 为整体执行超时（<=0 走默认 15 秒）；username/homeDir 必须已完成系统存在性校验。
func SyncCodexConfig(username, homeDir string, codex *auth.CodexConfig, timeout time.Duration) error {
	if codex == nil || !codex.Enabled {
		return nil
	}
	if username == "" || homeDir == "" {
		return errors.New("empty username or homeDir")
	}
	if codex.SyncFrom == "" {
		return errors.New("codex sync_from is empty")
	}

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("cannot find sudo: " + err.Error())
	}

	cmdStr := syncCodexCommand(username, homeDir, codex.SyncFrom, codex.OverwriteEnabled())
	cmd := exec.Command(sudoPath, "-n", "-u", username, "-i", "bash", "-c", cmdStr)
	cmd.Dir = "/"
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sync codex config failed: %v: %s", err, string(out))
	}
	return nil
}

// Read 从 pty master 读取输出字节。
func (s *Session) Read(buf []byte) (int, error) {
	// 注意：不要在持有 s.mu 的情况下阻塞在 master.Read 上，
	// 否则 Close 将永远无法获取锁来终止 bash 进程（死锁/进程泄漏）。
	// 因此仅在锁内读取 master 指针与 closed 状态，解锁后再阻塞读取。
	s.mu.Lock()
	master := s.master
	closed := s.closed
	s.mu.Unlock()

	if closed || master == nil {
		return 0, os.ErrClosed
	}
	return master.Read(buf)
}

// Write 向 pty master 写入输入字节。
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.master == nil {
		return 0, os.ErrClosed
	}
	return s.master.Write(p)
}

// Resize 调整 pty 终端尺寸。cols/rows 必须为正整数。
func (s *Session) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.master == nil {
		return os.ErrClosed
	}
	if cols == 0 || rows == 0 {
		return errors.New("invalid size")
	}
	return pty.Setsize(s.master, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
}

// Close 关闭 pty 会话，确保只执行一次。
// 先关闭 master，然后尝试终止并 Wait bash 进程。
// 注意：slave 已在 startWithPty 中于 Start 成功后立即关闭，这里无需再处理。
func (s *Session) Close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		master := s.master
		authEnvFile := s.authEnvFile
		s.mu.Unlock()

		// 关闭 master 会让 bash 读到 EOF 而退出。
		if master != nil {
			master.Close()
		}

		// 清理沙箱模式 opencode 的 api_key 环境文件（以目标用户身份删除）。
		if authEnvFile != "" {
			if err := RemoveAuthEnvFile(s.username, s.workDir); err != nil {
				log.Printf("warn: remove auth env file %s: %v", authEnvFile, err)
			}
		}

		if s.cmd != nil && s.cmd.Process != nil {
			// 若 bash 尚未退出，尝试终止它。
			_ = s.cmd.Process.Kill()
			// Wait 回收进程资源。
			_ = s.cmd.Wait()
		}
	})
}

// IsClosed 返回会话是否已关闭。
func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// procChildren 返回指定进程的直接子进程 PID 列表（读取 /proc/<pid>/task/<pid>/children）。
// 服务进程（ease）是 sudo 的父进程，因此有权读取其 children。
func procChildren(pid int) []int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	var out []int
	var n int
	for _, c := range b {
		if c == ' ' {
			out = append(out, n)
			n = 0
		} else if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return out
}

// procComm 返回指定进程的 comm 名称；读取失败返回空串。
func procComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// shellQuote 将字符串包进单引号，供拼接到 bash -c 命令行使用（规避特殊字符注入）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WorkingDirectory 返回会话当前工作目录（交互 shell 的实时 cwd）。
//
// 实现：在 sudo 进程树中定位交互 bash 进程，再用 sudo 以目标用户身份
// `readlink /proc/<pid>/cwd` 取得实时目录。服务用户（ease）无权直接读取他用户
// 进程的 cwd（EACCES），必须经 `sudo -u <user>` 降权读取；而进程树本身是服务
// 用户的子进程，可直接遍历。解析失败时回退到会话初始工作目录（homeDir）。
func (s *Session) WorkingDirectory() string {
	s.mu.Lock()
	pid := 0
	username := s.username
	fallback := s.workDir
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	s.mu.Unlock()

	if pid == 0 || username == "" {
		return fallback
	}

	// BFS 遍历进程树，找到交互 bash 进程，取其 cwd。
	queue := []int{pid}
	seen := make(map[int]bool)
	best := ""
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		if procComm(p) == "bash" {
			// 以目标用户身份读取其自身进程的 cwd。
			cwd, err := s.readCwdAsUser(username, p)
			if err == nil && cwd != "" {
				best = cwd
			}
		}
		queue = append(queue, procChildren(p)...)
	}
	if best != "" {
		return best
	}
	return fallback
}

// readCwdAsUser 以目标用户身份执行 `readlink /proc/<pid>/cwd` 取得目录。
func (s *Session) readCwdAsUser(username string, pid int) (string, error) {
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(sudoPath, "-n", "-u", username,
		"readlink", fmt.Sprintf("/proc/%d/cwd", pid))
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteFileAsUser 以目标用户身份将 data 写入 dir/filename（用于文件上传）。
// 文件将由目标用户创建并归属其名下；返回实际写入的绝对路径。
// 权限不足等错误会原样返回（调用方据此提示用户切换目录）。
func (s *Session) WriteFileAsUser(filename string, data []byte) (string, error) {
	s.mu.Lock()
	username := s.username
	s.mu.Unlock()
	if username == "" {
		return "", errors.New("empty username")
	}

	dir := s.WorkingDirectory()
	dest := dir + "/" + filename
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return "", errors.New("cannot find sudo")
	}
	// sudo -n -u <user> bash -c 'cat > <dest>'：以目标用户身份创建文件，
	// 内容经 stdin 传入。dest 由 /proc cwd 与 basename 拼接并做 shell 转义。
	cmd := exec.Command(sudoPath, "-n", "-u", username, "bash", "-c", "cat > "+shellQuote(dest))
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return dest, nil
}

// CheckWritableAsUser 以目标用户身份检测指定目录是否可写（用于上传前预检）。
// 通过在目标目录内创建并删除一个临时文件来验证，避免上传完成时才暴露权限问题。
// 返回该目录（可能为规范化路径）；不可写时返回错误（含权限/目录类信息）。
func (s *Session) CheckWritableAsUser(dir string) (string, error) {
	s.mu.Lock()
	username := s.username
	s.mu.Unlock()
	if username == "" {
		return "", errors.New("empty username")
	}
	if dir == "" {
		dir = s.WorkingDirectory()
	}

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return "", errors.New("cannot find sudo")
	}
	// 在目标目录创建唯一临时文件（.webshell_writecheck_<pid>），成功后删除。
	// 整体以 bash -c 执行，成功与否通过 stderr 判定；任何一步失败即视为不可写。
	testFile := dir + "/.webshell_writecheck_" + fmt.Sprintf("%d", os.Getpid())
	cmdStr := "touch " + shellQuote(testFile) + " && rm -f " + shellQuote(testFile)
	cmd := exec.Command(sudoPath, "-n", "-u", username, "bash", "-c", cmdStr)
	cmd.Dir = "/"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return dir, nil
}

// WriteAuthEnvFile 以目标用户身份写入沙箱 opencode 的 api_key 环境文件
// <homeDir>/.config/opencode/.auth.env（内容为 `export KEY='val'` 行，权限 0600）。
//
// 经 `sudo -u <user> bash -c 'umask 077; cat > <dest> && chmod 600 <dest>'` 执行，
// 内容经 stdin 传入（api_key 不进命令行参数）。沙箱内家目录 bind 到与宿主相同
// 相对路径，sandbox-init 降权后（目标用户身份）可直接读取该文件并注入环境。
//
// authEnv 为空时返回 ("", nil)，不做任何操作。返回实际写入的绝对路径。
func WriteAuthEnvFile(username, homeDir string, authEnv map[string]string) (string, error) {
	if len(authEnv) == 0 {
		return "", nil
	}
	if username == "" || homeDir == "" {
		return "", errors.New("empty username or homeDir")
	}

	// 排序保证文件内容确定性（便于排查与幂等）。
	keys := make([]string, 0, len(authEnv))
	for k := range authEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString("export " + k + "=" + shellQuote(authEnv[k]) + "\n")
	}

	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return "", errors.New("cannot find sudo: " + err.Error())
	}

	dest := homeDir + "/.config/opencode/.auth.env"
	cmd := exec.Command(sudoPath, "-n", "-u", username, "bash", "-c",
		"umask 077; cat > "+shellQuote(dest)+" && chmod 600 "+shellQuote(dest))
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(sb.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return dest, nil
}

// RemoveAuthEnvFile 以目标用户身份删除 <homeDir>/.config/opencode/.auth.env
// （会话结束时清理敏感文件；文件不存在时静默成功）。
func RemoveAuthEnvFile(username, homeDir string) error {
	if username == "" || homeDir == "" {
		return nil
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("cannot find sudo: " + err.Error())
	}
	dest := homeDir + "/.config/opencode/.auth.env"
	cmd := exec.Command(sudoPath, "-n", "-u", username, "bash", "-c", "rm -f "+shellQuote(dest))
	cmd.Dir = "/"
	return cmd.Run()
}
