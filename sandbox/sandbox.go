// Package sandbox 实现 Linux 命名空间沙箱，编译为独立的二进制 sandbox-init，
// 作为整个 webshell 服务唯一的提权入口。root 权限由部署侧 sudoers 规则
// （sudo 提权为 root）提供，二进制本身为 0755，无需 setuid-root。
//
// 方案 A：不使用 user namespace。流程为：
//   - 解析参数 -> 进入 cgroup -> 以宿主 root 预创建 rootfs 挂载点
//   - exec 单线程 C 程序 userns-init（保持宿主 root）：unshare(NS/PID/[NET]) +
//     make-rprivate + fork，子进程成为新 PID namespace 的 PID 1
//   - re-exec 本二进制（--child，宿主 root）-> 组装新根 -> pivot_root
//   - rlimit -> seccomp -> setgroups(主组+补充组)/setgid/setuid 降权到目标真实
//     用户（保留补充组，使 NFS 等依赖组权限的访问与宿主机一致）-> exec bash/opencode。
//
// 沙箱内进程最终是宿主目标用户（uid/gid/补充组），不是 root，也不是 userns root。
//
// 本包是独立二进制（package main），不 import webshell 的其他包。
//
// 实现说明：Go 运行时是多线程的，无法安全地使用原生 fork()。因此"unshare +
// fork"由单线程 C 程序 userns-init 完成：父进程先 unshare(CLONE_NEWPID)，再
// fork + exec 本二进制（带 --child 标记）生成子进程。由于 unshare(CLONE_NEWPID)
// 只对之后创建的子进程生效，该子进程即成为新 PID namespace 中的 PID 1（init），
// 并继承父进程已有的 mount/net namespace。
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Config 保存 sandbox-init 的一次运行参数。
type Config struct {
	UID          int             // 真实用户 UID（正整数，非 0）
	GID          int             // 真实用户 GID（正整数，非 0）
	Groups       []int           // 真实用户的补充组 GID 列表（含主组，降权时 setgroups 保留）
	Home         string          // 真实家目录（绝对路径，以 /home/ 开头）
	Session      string          // 会话 ID（^[a-zA-Z0-9_-]+$，用于 cgroup 名，防路径注入）
	Program      string          // bash | opencode
	OpencodePath string          // --program=opencode 时的可执行文件绝对路径
	Rootfs       string          // base rootfs 路径
	Network      string          // none | loopback | full
	Memory       string          // cgroup memory.max（如 "512M"）
	CPU          string          // cgroup cpu.max（如 "50000 100000"）
	Pids         int             // cgroup pids.max
	Seccomp      bool            // 是否安装 seccomp 过滤器
	TmpSize      string          // /tmp tmpfs 大小（如 "256M"）
	NoFile       int             // RLIMIT_NOFILE
	CoreDump     bool            // false 时 RLIMIT_CORE=0
	MaxFsize     string          // RLIMIT_FSIZE（如 "1G"）
	ProcHidepid  bool            // /proc 挂载 hidepid=2
	DevList      []string        // 需要 mknod 的设备类型列表（预留，默认最小集）
	CgroupRoot   string          // cgroup v2 根目录（/<session> 子目录作为该会话的 cgroup）
	BindHosts    bool            // 是否把宿主机 /etc/hosts 映射到沙箱内 /etc/hosts
	BindMounts   []BindMountSpec // 宿主路径 bind 到沙箱路径的列表（如 NFS 挂载点）
}

// BindMountSpec 表示一个"宿主路径 bind 到沙箱路径"的挂载。
type BindMountSpec struct {
	Source string // 宿主绝对路径（可含已挂载的 NFS）
	Target string // 沙箱内绝对路径
}

// Run 执行完整的沙箱初始化流程。仅当配置校验失败时返回 error；正常流程以
// exec 结束（不会返回）。
func Run(cfg *Config) error {
	if err := validate(cfg); err != nil {
		return err
	}

	// 子进程（re-exec 带 --child）负责完整的根文件系统 / 降权 / exec。
	if isChild() {
		return childInit(cfg)
	}

	// 进入 cgroup。必须在 unshare(CLONE_NEWNS|CLONE_NEWPID) 之前、仍为宿主
	// root 时执行，否则进入新 namespace/降权后进程失去宿主 CAP_SYS_ADMIN，
	// 无法写宿主 cgroup v2 的 cgroup.procs。父进程进入后，后续 fork/exec 的
	// 所有子进程都会自动继承同一 cgroup，子进程无需（也无权限）再次进入。
	enterCgroup(cfg)

	// 以宿主 root 预创建 rootfs 内的挂载点目录。childInit 以 root 运行其实也能
	// 写 rootfs，但保留此步骤无害，且确保挂载点先于 buildRootFS 存在。
	// 家目录挂载点（<rootfs><cfg.Home 相对路径>）、tmp、dev、proc、oldroot
	// 均在此预创建，供 buildRootFS 挂载家目录 / tmpfs / dev / proc 使用。
	homeRel := strings.TrimPrefix(cfg.Home, "/") // 如 share/easemgt/ushij067
	for _, d := range []string{
		filepath.Join(cfg.Rootfs, homeRel),
		filepath.Join(cfg.Rootfs, "tmp"),
		filepath.Join(cfg.Rootfs, "dev"),
		filepath.Join(cfg.Rootfs, "dev", "pts"),
		filepath.Join(cfg.Rootfs, "dev", "shm"),
		filepath.Join(cfg.Rootfs, "proc"),
		filepath.Join(cfg.Rootfs, "oldroot"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("precreate rootfs mountpoint %s: %w", d, err)
		}
	}
	// bind 挂载点（如 NFS 挂载点）的目标目录同样以宿主 root 预创建，供
	// buildRootFS 直接 bind 使用。
	for _, m := range cfg.BindMounts {
		d := filepath.Join(cfg.Rootfs, strings.TrimPrefix(m.Target, "/"))
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("precreate rootfs bind mountpoint %s: %w", d, err)
		}
	}
	// /dev 设备占位文件：/dev 通过 bind 宿主设备节点组装，故先以 root 创建
	// 普通占位文件作为 bind 挂载点。
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty", "ptmx"} {
		p := filepath.Join(cfg.Rootfs, "dev", name)
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
			_ = f.Close()
		}
	}

	// 解析目标用户的补充组（含主组），供 childInit 降权时 setgroups 保留。
	// 方案 A 移除 userns 后，进程最终以宿主目标用户身份运行，必须保留补充组，
	// 否则 NFS 等依赖组权限的 ACL 检查会因丢失补充组而 Permission denied。
	cfg.Groups = resolveSupplementaryGroups(cfg)

	// 以宿主 root 身份 exec 单线程 C 程序（userns-init，不降权），由它完成
	// unshare NS/PID/[NET]、make-rprivate、fork 并 exec 回本二进制（--child）。
	return execUsernsInit(cfg)
}

// isChild 判断当前进程是否由 re-exec 以 --child 标记启动。
func isChild() bool {
	for _, a := range os.Args[1:] {
		if a == "--child" {
			return true
		}
	}
	return false
}

// childArgs 构造子进程 re-exec 的参数：原始参数 + --child 标记。
func childArgs(cfg *Config) []string {
	args := []string{
		"--uid", strconv.Itoa(cfg.UID),
		"--gid", strconv.Itoa(cfg.GID),
		"--home", cfg.Home,
		"--session", cfg.Session,
		"--program", cfg.Program,
		"--rootfs", cfg.Rootfs,
		"--network", cfg.Network,
		"--tmp-size", cfg.TmpSize,
		"--nofile", strconv.Itoa(cfg.NoFile),
		"--max-fsize", cfg.MaxFsize,
		"--cgroup-root", cfg.CgroupRoot,
		"--seccomp=" + strconv.FormatBool(cfg.Seccomp),
		"--core-dump=" + strconv.FormatBool(cfg.CoreDump),
		"--proc-hidepid=" + strconv.FormatBool(cfg.ProcHidepid),
	}
	// 补充组列表（含主组）随 re-exec 参数传给 child，childInit 降权时
	// setgroups 保留，使 NFS 等依赖组权限的访问与宿主机一致。
	if len(cfg.Groups) > 0 {
		gs := make([]string, 0, len(cfg.Groups))
		for _, g := range cfg.Groups {
			gs = append(gs, strconv.Itoa(g))
		}
		args = append(args, "--groups", strings.Join(gs, ","))
	}
	if cfg.Memory != "" {
		args = append(args, "--memory", cfg.Memory)
	}
	if cfg.CPU != "" {
		args = append(args, "--cpu", cfg.CPU)
	}
	if cfg.Pids > 0 {
		args = append(args, "--pids", strconv.Itoa(cfg.Pids))
	}
	if cfg.Program == "opencode" {
		args = append(args, "--opencode-path", cfg.OpencodePath)
	}
	if cfg.BindHosts {
		args = append(args, "--bind-hosts")
	}
	for _, m := range cfg.BindMounts {
		args = append(args, "--bind-mount", m.Source, "--bind-mount-target", m.Target)
	}
	args = append(args, "--child")
	return args
}

// execUsernsInit 以宿主 root 身份 exec 单线程 C 程序 userns-init，由它完成
// unshare(CLONE_NEWNS|CLONE_NEWPID|[CLONE_NEWNET]) + make-rprivate，再 fork +
// exec 回本二进制（/proc/self/exe，带 --child 参数）进入 childInit。
//
// C 程序必须由本进程（宿主 root）执行：方案 A 不使用 userns，需 root 身份
// unshare NS/PID/NET。补充组列表不传给 C（C 不再降权），而是随 childArgs
// 放在 "--" 之后由 child（sandbox-init --child）解析使用。
func execUsernsInit(cfg *Config) error {
	const usernsInitPath = "/opt/webshell_sandbox/userns-init"

	// C 程序参数：argv[0]=程序名，后接命名空间创建所需信息 + -- 分隔后的
	// sandbox-init --child 参数。childArgs 已含 --child 标记；C 程序最终
	// fork + exec /proc/self/exe。
	args := []string{
		usernsInitPath, // argv[0] 必须是程序名
		"--uid", strconv.Itoa(cfg.UID),
		"--gid", strconv.Itoa(cfg.GID),
		"--session", cfg.Session,
		"--cgroup-root", cfg.CgroupRoot,
	}
	// network=none/loopback 都禁止外部网络访问（loopback 回环在 childInit 中 up lo）。
	if cfg.Network != "full" {
		args = append(args, "--no-net")
	}
	args = append(args, "--")

	selfExe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("readlink /proc/self/exe: %w", err)
	}
	args = append(args, selfExe)
	args = append(args, childArgs(cfg)...)

	// 直接 exec 替换当前进程（不额外 fork，避免多余层级）。C 程序内部会 fork。
	return unix.Exec(usernsInitPath, args, os.Environ())
}

// resolveSupplementaryGroups 返回目标用户应具备的组 ID 列表（含主组 GID），
// 供 childInit 降权时 setgroups 使用。
//
// 通过 os/user 查询系统用户：先 user.Lookup(用户名) 再取 GroupIds()（读取
// /etc/group 得出该用户所属的所有组，通常已含主组），再与主组 cfg.GID 去重
// 合并，确保 Groups 至少包含主组 GID。解析失败不阻断沙箱（回退为仅主组），
// 但以 ERROR 告警——此时 NFS 等依赖补充组的访问可能受限，与宿主机行为不一致。
func resolveSupplementaryGroups(cfg *Config) []int {
	groups := []int{cfg.GID}
	username := filepath.Base(cfg.Home)

	u, err := user.Lookup(username)
	if err != nil {
		log.Printf("ERROR: resolve supplementary groups: lookup %q: %v (fallback to primary gid %d)", username, err, cfg.GID)
		return groups
	}
	gidStrs, err := u.GroupIds()
	if err != nil {
		log.Printf("ERROR: resolve supplementary groups: GroupIds(%q): %v (fallback to primary gid %d)", username, err, cfg.GID)
		return groups
	}

	seen := map[int]bool{cfg.GID: true}
	for _, s := range gidStrs {
		g, err := strconv.Atoi(s)
		if err != nil || g <= 0 || seen[g] {
			continue
		}
		seen[g] = true
		groups = append(groups, g)
	}
	log.Printf("supplementary groups for %s: %v", username, groups)
	return groups
}

// dropToUser 降权到目标真实用户，保留补充组（含主组），使 NFS 等依赖组权限的
// 访问与宿主机一致。在 childInit 中执行，此时进程为宿主 root、已安装 seccomp。
//
// 顺序：先 setgroups（需 CAP_SETGID，且必须早于 setuid，setuid 后失去
// CAP_SETGID 无法再改组），再 setgid（设主组），最后 setuid（丢弃 root）。
// setuid 后进程是普通用户且无 CAP_SETUID/CAP_SETGID，即使 seccomp 放行
// setuid/setgid 也无法提权回 root（只能设为自身 uid/gid，无实际作用）。
func dropToUser(cfg *Config) error {
	// 组合最终组列表：主组 cfg.GID + 全部补充组（cfg.Groups 已含主组，去重）。
	gids := make([]int, 0, len(cfg.Groups))
	seen := make(map[int]bool)
	add := func(g int) {
		if g <= 0 || seen[g] {
			return
		}
		seen[g] = true
		gids = append(gids, g)
	}
	add(cfg.GID)
	for _, g := range cfg.Groups {
		add(g)
	}
	if err := unix.Setgroups(gids); err != nil {
		return fmt.Errorf("setgroups %v: %w", gids, err)
	}
	if err := unix.Setgid(cfg.GID); err != nil {
		return fmt.Errorf("setgid %d: %w", cfg.GID, err)
	}
	if err := unix.Setuid(cfg.UID); err != nil {
		return fmt.Errorf("setuid %d: %w", cfg.UID, err)
	}
	return nil
}

// validate 校验所有命令行参数，非法即 fail-fast 返回 error。
func validate(cfg *Config) error {
	if cfg.UID <= 0 {
		return errors.New("uid must be a positive non-zero integer")
	}
	if cfg.GID <= 0 {
		return errors.New("gid must be a positive non-zero integer")
	}
	if !filepath.IsAbs(cfg.Home) {
		return errors.New("home must be an absolute path")
	}
	sessionRe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	if !sessionRe.MatchString(cfg.Session) {
		return errors.New("session contains invalid characters")
	}
	switch cfg.Program {
	case "bash", "opencode":
	default:
		return errors.New("program must be 'bash' or 'opencode'")
	}
	if cfg.Program == "opencode" && cfg.OpencodePath == "" {
		return errors.New("opencode path is empty")
	}
	switch cfg.Network {
	case "none", "loopback", "full":
	default:
		return errors.New("network must be none|loopback|full")
	}
	if !filepath.IsAbs(cfg.Rootfs) {
		return errors.New("rootfs must be an absolute path")
	}
	for _, m := range cfg.BindMounts {
		if !filepath.IsAbs(m.Source) {
			return errors.New("bind mount source must be an absolute path")
		}
		if !filepath.IsAbs(m.Target) {
			return errors.New("bind mount target must be an absolute path")
		}
	}
	if cfg.NoFile <= 0 {
		cfg.NoFile = 4096
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup/webshell_sandbox"
	}
	return nil
}

// childInit 由 re-exec 的子进程（新 PID namespace 的 PID 1，以宿主 root 身份
// 运行）执行：组装新根 -> pivot_root -> rlimit -> seccomp -> 降权到目标真实
// 用户（保留补充组）-> exec。
func childInit(cfg *Config) error {
	// 注意：不在此处进入 cgroup。父进程已在 unshare(NS/PID/NET) 之前以宿主
	// root 身份进入，子进程继承同一 cgroup。此处为宿主 root（方案 A 不使用
	// userns），先以 root 完成 buildRootFS/pivot_root，再在最后降权到目标
	// 用户——降权必须在 buildRootFS/pivot/seccomp 之后，否则无 root 权限
	// 无法组装根文件系统。

	// mount namespace 内所有挂载改为私有，避免影响宿主及其他容器。
	// （userns-init.c 已 make-rprivate，此处幂等保留。）
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("mount --make-rprivate /: %w", err)
	}

	// 组装新根：临时目录作为 new_root。
	root, err := buildRootFS(cfg)
	if err != nil {
		return fmt.Errorf("build rootfs: %w", err)
	}
	defer os.RemoveAll(root) // 清理失败时的残留 staging（正常流程 pivot_root 后不可达）

	// pivot_root：new_root 与 put_old 需在不同 mount 上（staging 已单独 bind）。
	oldRoot := filepath.Join(root, "oldroot")
	if err := unix.PivotRoot(root, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	// 卸载旧根并移除挂载点。
	if err := unix.Unmount("/oldroot", unix.MNT_DETACH); err != nil {
		log.Printf("warn: unmount /oldroot: %v", err)
	}
	_ = os.Remove("/oldroot")

	// rlimit。
	if err := applyRlimits(cfg); err != nil {
		return fmt.Errorf("apply rlimits: %w", err)
	}

	// seccomp。
	if cfg.Seccomp {
		if err := installSeccomp(); err != nil {
			return fmt.Errorf("install seccomp: %w", err)
		}
	}

	// loopback 模式：userns_init.c 已创建 netns，这里把回环接口 up，使 lo 可用。
	// 需在降权之前执行（up 需要 netns 内 CAP_NET_ADMIN）。
	if cfg.Network == "loopback" {
		if err := upLoopback(); err != nil {
			return fmt.Errorf("up loopback: %w", err)
		}
	}

	// 降权到目标真实用户，保留补充组（含主组）。必须在 seccomp 安装与
	// loopback up 之后：此后进程不再需要 root 权限，且以目标用户身份运行使
	// NFS 等依赖组权限的访问与宿主机一致（解决 userns 丢失补充组的问题）。
	if err := dropToUser(cfg); err != nil {
		return fmt.Errorf("drop to user: %w", err)
	}

	// exec 目标程序。
	return execProgram(cfg)
}

// enterCgroup 将当前进程写入 <cgroup-root>/<session>/cgroup.procs。
// 必须在 unshare(NS/PID/NET) 之前、进程仍为宿主 root 时调用，否则无权限
// 写宿主 cgroup v2。best-effort：失败不阻塞会话，但必须以 ERROR 级别告警，
// 因为 cgroup 失效意味着 memory/cpu/pids 资源限制不生效，属于安全隐患。
func enterCgroup(cfg *Config) {
	if cfg.CgroupRoot == "" {
		return
	}
	dir := filepath.Join(cfg.CgroupRoot, cfg.Session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("ERROR: cgroup mkdir %s: %v (resource limits will NOT be enforced)", dir, err)
		return
	}
	pid := os.Getpid()
	procs := filepath.Join(dir, "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		log.Printf("ERROR: cgroup write %s: %v (resource limits will NOT be enforced)", procs, err)
		return
	}
	// 应用资源限制。
	applyCgroupLimits(dir, cfg)
	// 确认并记录该会话实际使用的 cgroup 路径，便于排查。
	log.Printf("cgroup: entered %s (pid %d)", dir, pid)
}

// applyCgroupLimits 写入 memory.max / cpu.max / pids.max。
func applyCgroupLimits(dir string, cfg *Config) {
	if cfg.Memory != "" {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(cfg.Memory+"\n"), 0o644); err != nil {
			log.Printf("ERROR: cgroup memory.max: %v (memory limit NOT enforced)", err)
		}
	}
	if cfg.CPU != "" {
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(cfg.CPU+"\n"), 0o644); err != nil {
			log.Printf("ERROR: cgroup cpu.max: %v (cpu limit NOT enforced)", err)
		}
	}
	if cfg.Pids > 0 {
		if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte(strconv.Itoa(cfg.Pids)+"\n"), 0o644); err != nil {
			log.Printf("ERROR: cgroup pids.max: %v (pids limit NOT enforced)", err)
		}
	}
}

// buildRootFS 组装新根目录树，返回 new_root 的路径。
//
// 流程：
//  1. 创建 staging 临时目录；
//  2. bind（递归）base rootfs 到 staging 作为新根；
//  3. 在新根内创建 home/tmp/dev/proc/oldroot 目录；
//  4. bind 真实家目录 -> staging/home/<user>；
//  5. tmpfs -> staging/tmp；
//  6. tmpfs -> staging/dev + mknod 最小设备 + mkdir pts；
//  7. proc -> staging/proc（hidepid 可选）；
//  8. bind 配置的宿主路径（BindMounts，如 NFS 挂载点）与 /etc/hosts（BindHosts）；
//  9. 将 staging（base rootfs）remount 为只读（不影响已建子挂载）。
func buildRootFS(cfg *Config) (string, error) {
	staging, err := os.MkdirTemp("", "sandbox-init-")
	if err != nil {
		return "", fmt.Errorf("mkdtemp: %w", err)
	}

	// bind base rootfs 到 staging（递归，含根内已有子挂载）。
	if err := unix.Mount(cfg.Rootfs, staging, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return "", fmt.Errorf("bind rootfs: %w", err)
	}

	// 创建新根内的挂载点目录（此时 staging 可写）。家目录挂载到新根内与宿主
	// 相同的相对路径（staging + cfg.Home 去掉前导 /），使用户看到路径与宿主一致。
	homeUser := filepath.Join(staging, strings.TrimPrefix(cfg.Home, "/"))
	for _, d := range []string{homeUser, filepath.Join(staging, "tmp"),
		filepath.Join(staging, "dev"), filepath.Join(staging, "proc"),
		filepath.Join(staging, "oldroot")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// bind 真实家目录（读写）。
	if err := unix.Mount(cfg.Home, homeUser, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return "", fmt.Errorf("bind home: %w", err)
	}

	// bind 宿主已挂载路径（如 NFS 挂载点）到沙箱内对应路径。目标目录已在宿主
	// root 阶段预创建（Run 的 precreate），此处直接 bind。失败即返回 error，
	// 让明确配置的挂载失败可见、会话启动失败。
	for _, m := range cfg.BindMounts {
		dst := filepath.Join(staging, strings.TrimPrefix(m.Target, "/"))
		if err := unix.Mount(m.Source, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return "", fmt.Errorf("bind %s -> %s: %w", m.Source, dst, err)
		}
	}

	// 映射宿主机 /etc/hosts 到沙箱（解决沙箱内主机名解析）。目标文件
	// /etc/hosts 在 base rootfs 中已存在。
	if cfg.BindHosts {
		if err := unix.Mount("/etc/hosts", filepath.Join(staging, "etc", "hosts"), "", unix.MS_BIND, ""); err != nil {
			return "", fmt.Errorf("bind /etc/hosts: %w", err)
		}
	}

	// tmpfs /tmp。
	if err := unix.Mount("tmpfs", filepath.Join(staging, "tmp"), "tmpfs", 0,
		"size="+cfg.TmpSize+",mode=1777"); err != nil {
		return "", fmt.Errorf("mount tmpfs /tmp: %w", err)
	}

	// 最小 /dev。
	if err := mountDev(filepath.Join(staging, "dev")); err != nil {
		return "", fmt.Errorf("mount /dev: %w", err)
	}

	// proc。
	procOpts := ""
	if cfg.ProcHidepid {
		procOpts = "hidepid=2"
	}
	if err := unix.Mount("proc", filepath.Join(staging, "proc"), "proc", 0, procOpts); err != nil {
		return "", fmt.Errorf("mount proc: %w", err)
	}

	// 将 base rootfs 只读化（仅作用于 staging 这个 mount，不影响子挂载）。
	if err := unix.Mount("", staging, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return "", fmt.Errorf("remount rootfs readonly: %w", err)
	}

	return staging, nil
}

// mountDev 组装 /dev：/dev 设备节点通过 bind 宿主设备节点到沙箱 /dev 实现。
// 挂载点（占位文件/目录）已由父进程在 root 阶段预创建。
func mountDev(devPath string) error {
	// bind 宿主设备节点到沙箱 /dev（目标占位文件已预创建）。
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty", "ptmx"} {
		if err := unix.Mount("/dev/"+name, filepath.Join(devPath, name), "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind /dev/%s: %w", name, err)
		}
	}
	// pts/shm 目录（用于 pty / 共享内存）。
	if err := os.MkdirAll(filepath.Join(devPath, "pts"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(devPath, "shm"), 0o1777); err != nil {
		return err
	}
	return nil
}

// parseBytes 将 "1G"/"512M"/"1024K"/"1048576" 形式的字符串解析为字节数。
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := uint64(1)
	last := s[len(s)-1]
	switch last {
	case 'g', 'G':
		mult = 1 << 30
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'k', 'K':
		mult = 1 << 10
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// applyRlimits 应用 rlimit：nofile / nproc / core / fsize / stack。
//
// 所有 rlimit 均为 best-effort：沙箱内可能无法设置某些软限制（如
// RLIMIT_CORE=0 会返回 operation not permitted）。这些是加固/资源限制项，
// 失败不应拒绝启动沙箱——沙箱隔离（pivot_root/seccomp/cgroup）仍然有效，
// 只是某个软限制未生效。因此任一设置失败时仅记录警告并继续，函数最终总是返回 nil。
func applyRlimits(cfg *Config) error {
	r := unix.Rlimit{Cur: uint64(cfg.NoFile), Max: uint64(cfg.NoFile)}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &r); err != nil {
		log.Printf("warn: setrlimit nofile: %v (continuing)", err)
	}

	// nproc 与 pids 相关；未指定时给一个宽松默认（内部参考 pids 或 4096）。
	nproc := uint64(4096)
	if cfg.Pids > 0 {
		nproc = uint64(cfg.Pids) * 4
	}
	r = unix.Rlimit{Cur: nproc, Max: nproc}
	if err := unix.Setrlimit(unix.RLIMIT_NPROC, &r); err != nil {
		log.Printf("warn: setrlimit nproc: %v (continuing)", err)
	}

	// core dump：--core-dump=false 时 RLIMIT_CORE=0。
	core := uint64(0)
	if cfg.CoreDump {
		core = 1 << 30 // 1G
	}
	r = unix.Rlimit{Cur: core, Max: core}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &r); err != nil {
		log.Printf("warn: setrlimit core: %v (continuing)", err)
	}

	// fsize：默认 1G。
	fsize, err := parseBytes(cfg.MaxFsize)
	if err != nil {
		fsize = 1 << 30
	}
	r = unix.Rlimit{Cur: fsize, Max: fsize}
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &r); err != nil {
		log.Printf("warn: setrlimit fsize: %v (continuing)", err)
	}

	// stack：8M。
	r = unix.Rlimit{Cur: 8 << 20, Max: 8 << 20}
	if err := unix.Setrlimit(unix.RLIMIT_STACK, &r); err != nil {
		log.Printf("warn: setrlimit stack: %v (continuing)", err)
	}
	return nil
}

// installSeccomp 安装黑名单式 seccomp BPF 过滤器，拦截特权/危险系统调用并返回 EPERM。
func installSeccomp() error {
	// 黑名单系统调用号（按架构解析）。
	blacklist := []uintptr{
		unix.SYS_UNSHARE,
		unix.SYS_SETNS,
		unix.SYS_MOUNT,
		unix.SYS_UMOUNT2,
		unix.SYS_PIVOT_ROOT,
		unix.SYS_CHROOT,
		unix.SYS_INIT_MODULE,
		unix.SYS_FINIT_MODULE,
		unix.SYS_DELETE_MODULE,
		unix.SYS_KEXEC_LOAD,
		unix.SYS_REBOOT,
		unix.SYS_SWAPON,
		unix.SYS_SWAPOFF,
		// 注意：SYS_SETUID/SYS_SETGID 不在黑名单中——childInit 需用它们降权
		// 到目标真实用户。降权后进程是普通用户、无 CAP_SETUID/CAP_SETGID，
		// 只能把 uid/gid 设为自身，无法提权，安全。SYS_SETGROUPS 同理。
		unix.SYS_KEYCTL,
		unix.SYS_ADD_KEY,
		unix.SYS_REQUEST_KEY,
		unix.SYS_BPF,
		unix.SYS_USERFAULTFD,
		unix.SYS_IOPERM,
		unix.SYS_IOPL,
		// 新版挂载 API（内核 5.15 起）：root + CAP_SYS_ADMIN 下可用，
		// 是绕过 mount 黑名单的高危逃逸面，必须一并拦截。
		unix.SYS_OPEN_TREE,
		unix.SYS_MOVE_MOUNT,
		unix.SYS_FSOPEN,
		unix.SYS_FSCONFIG,
		unix.SYS_FSMOUNT,
		unix.SYS_FSPICK,
		// 文件句柄：可跨目录打开任意文件。
		unix.SYS_OPEN_BY_HANDLE_AT,
		unix.SYS_NAME_TO_HANDLE_AT,
		// 设备：沙箱内降权前是 root，可 mknod 新设备（降权后无 CAP_MKNOD）。
		// 注意：SYS_MKNOD 在部分架构（arm64/riscv64）不存在，仅使用 SYS_MKNODAT；
		// 当前构建目标 linux/amd64 二者均存在，故直接列入。
		unix.SYS_MKNOD,
		unix.SYS_MKNODAT,
		// 调试/提权：跨进程。
		unix.SYS_PTRACE,
		unix.SYS_PERSONALITY,
		unix.SYS_PROCESS_VM_READV,
		unix.SYS_PROCESS_VM_WRITEV,
	}

	// 安装过滤器前进程仍为宿主 root，无需 PR_SET_NO_NEW_PRIVS；但加一层更稳妥。
	// 注意：PR_SET_NO_NEW_PRIVS 与 setuid 后安装冲突，这里不加，交由 root 安装。

	filters := buildFilter(blacklist)
	// 复制到可被内核读取的内存。
	prog := unix.SockFprog{
		Len:    uint16(len(filters)),
		Filter: &filters[0],
	}
	// PR_SET_SECCOMP(SECCOMP_MODE_FILTER, &prog)
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER,
		uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("prctl seccomp: %w", err)
	}
	return nil
}

// buildFilter 构造 BPF 程序：加载 syscall nr，逐一比对黑名单，命中返回
// SECCOMP_RET_ERRNO|EPERM，否则 ALLOW。
//
// 布局（n 为黑名单长度）：
//
//	[0]        loadNR          加载 seccomp_data.nr
//	[1..n]     JEQ nr_i        每个黑名单系统调用（Jt=n 命中跳转到自己的 deny）
//	[n+1]      allow
//	[n+2..2n+1] deny_0..deny_{n-1}
//
// 第 i 个 JEQ 位于索引 1+i，其 deny 位于索引 n+2+i；
// 命中时目标 = (1+i)+1+Jt = n+2+i => Jt = n；未命中 (Jf=0) 落到下一条。
func buildFilter(blacklist []uintptr) []unix.SockFilter {
	const (
		// 架构校验略去（简化），仅比对 syscall nr。
		offNR     = 0 // seccomp_data.nr 偏移
		bpfLdW    = uint16(unix.BPF_LD | unix.BPF_W | unix.BPF_ABS)
		bpfJmpJeq = uint16(unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K)
		bpfRet    = uint16(unix.BPF_RET | unix.BPF_K)
	)

	n := len(blacklist)
	loadNR := unix.SockFilter{Code: bpfLdW, K: offNR}
	allow := unix.SockFilter{Code: bpfRet, K: unix.SECCOMP_RET_ALLOW}
	deny := unix.SockFilter{Code: bpfRet, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)}

	filters := make([]unix.SockFilter, 0, n+2+n)
	filters = append(filters, loadNR)
	for _, nr := range blacklist {
		filters = append(filters, unix.SockFilter{
			Code: bpfJmpJeq,
			Jt:   uint8(n),
			Jf:   0,
			K:    uint32(nr),
		})
	}
	filters = append(filters, allow)
	for range blacklist {
		filters = append(filters, deny)
	}
	return filters
}

// upLoopback 将 lo 回环接口置为 IFF_UP。userns_init.c 创建的 netns 中回环
// 接口默认 down，若不 up 则 127.0.0.1 等回环地址不可达。仅 loopback 模式调用
// （none 模式同样禁网但无需回环；full 模式保留宿主网络不涉及新 netns）。
// 需在降权前执行（up 需要 netns 内 CAP_NET_ADMIN）。
func upLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("ifreq lo: %w", err)
	}

	// 获取当前 lo 标志。
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCGIFFLAGS: %w", err)
	}

	// 置位 IFF_UP（保留其余标志），再写回。
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCSIFFLAGS: %w", err)
	}
	return nil
}

// execProgram 以真实用户身份执行 bash 或 opencode，并设置环境。
func execProgram(cfg *Config) error {
	var argv []string
	var path string
	switch cfg.Program {
	case "opencode":
		path = cfg.OpencodePath
		argv = []string{cfg.OpencodePath}
	default:
		path = "/bin/bash"
		argv = []string{"/bin/bash", "-i"}
	}
	env := []string{
		"HOME=" + cfg.Home,
		"USER=" + filepath.Base(cfg.Home),
		"USERNAME=" + filepath.Base(cfg.Home),
		"LOGNAME=" + filepath.Base(cfg.Home),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"PS1=" + filepath.Base(cfg.Home) + `@\h:\w\$ `,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=en_US.utf8",
		"LC_ALL=en_US.utf8",
	}

	// 切换到用户家目录，使 bash/opencode 从用户的工程目录启动（与非沙箱路径
	// sudo -i 进入家目录保持一致）。此处已在 pivot_root 后的新根内，cfg.Home
	// 是被 bind 挂载到新根内与宿主相同相对路径（<newroot><cfg.Home>）的真实
	// 家目录，因此 chdir 到 cfg.Home 会成功；失败（如家目录不可用）时回退到 "/"。
	if err := unix.Chdir(cfg.Home); err != nil {
		log.Printf("warn: chdir %s: %v, fallback to /", cfg.Home, err)
		if err := unix.Chdir("/"); err != nil {
			return fmt.Errorf("chdir /: %w", err)
		}
	}

	return unix.Exec(path, argv, env)
}
