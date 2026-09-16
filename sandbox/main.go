package main

import (
	"flag"
	"log"
	"strconv"
	"strings"
)

// strList 实现 flag.Value，用于累积可重复出现的字符串参数
// （如 --bind-mount / --bind-mount-target）。
type strList []string

func (s *strList) String() string { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	cfg := &Config{}

	// 用 flag 包解析参数；--seccomp / --core-dump / --proc-hidepid 使用 Bool 布尔值。
	flag.IntVar(&cfg.UID, "uid", 0, "real user UID (positive, non-zero)")
	flag.IntVar(&cfg.GID, "gid", 0, "real user GID (positive, non-zero)")
	groups := flag.String("groups", "", "comma-separated supplementary GIDs including primary (internal, re-exec)")
	flag.StringVar(&cfg.Home, "home", "", "real home directory (absolute, under /home/)")
	flag.StringVar(&cfg.Session, "session", "", "session ID ([a-zA-Z0-9_-]+)")
	flag.StringVar(&cfg.Program, "program", "", "bash|opencode")
	flag.StringVar(&cfg.OpencodePath, "opencode-path", "", "opencode executable path")
	flag.StringVar(&cfg.AuthEnvFile, "auth-env-file", "", "auth env file with export KEY='val' lines (injected into opencode env)")
	flag.StringVar(&cfg.Rootfs, "rootfs", "", "base rootfs path")
	flag.StringVar(&cfg.Network, "network", "none", "none|loopback|full")
	flag.StringVar(&cfg.Memory, "memory", "", "cgroup memory.max (e.g. 512M)")
	flag.StringVar(&cfg.CPU, "cpu", "", "cgroup cpu.max (e.g. 50000 100000)")
	flag.IntVar(&cfg.Pids, "pids", 0, "cgroup pids.max")
	flag.StringVar(&cfg.TmpSize, "tmp-size", "256M", "tmpfs size for /tmp")
	flag.IntVar(&cfg.NoFile, "nofile", 4096, "RLIMIT_NOFILE")
	flag.StringVar(&cfg.MaxFsize, "max-fsize", "1G", "RLIMIT_FSIZE")
	flag.StringVar(&cfg.CgroupRoot, "cgroup-root", "/sys/fs/cgroup/webshell_sandbox", "cgroup v2 root")
	seccomp := flag.Bool("seccomp", true, "install seccomp filter")
	coreDump := flag.Bool("core-dump", false, "allow core dumps")
	procHidepid := flag.Bool("proc-hidepid", true, "mount /proc with hidepid=2")
	bindHosts := flag.Bool("bind-hosts", false, "bind host /etc/hosts into sandbox /etc/hosts")
	bindResolv := flag.Bool("bind-resolv", false, "bind host /etc/resolv.conf into sandbox /etc/resolv.conf")
	usernsInitPath := flag.String("userns-init-path", "", "userns-init executable path (default: derived as <sandbox-init dir>/userns-init)")
	httpProxy := flag.String("http-proxy", "", "http proxy URL injected into sandbox env (http_proxy)")
	httpsProxy := flag.String("https-proxy", "", "https proxy URL injected into sandbox env (https_proxy)")
	noProxy := flag.String("no-proxy", "", "no_proxy list injected into sandbox env")
	var bindMountSrcs, bindMountTargets strList
	flag.Var(&bindMountSrcs, "bind-mount", "host source path to bind (paired with --bind-mount-target)")
	flag.Var(&bindMountTargets, "bind-mount-target", "sandbox target path (paired with --bind-mount)")
	child := flag.Bool("child", false, "internal: re-exec child marker")
	flag.Parse()

	cfg.Seccomp = *seccomp
	cfg.CoreDump = *coreDump
	cfg.ProcHidepid = *procHidepid
	cfg.BindHosts = *bindHosts
	cfg.BindResolv = *bindResolv
	cfg.UsernsInitPath = *usernsInitPath
	cfg.HTTPProxy = *httpProxy
	cfg.HTTPSProxy = *httpsProxy
	cfg.NoProxy = *noProxy
	// --bind-mount 与 --bind-mount-target 必须成对出现（每对为一个 BindMountSpec）。
	if len(bindMountSrcs) != len(bindMountTargets) {
		log.Fatalf("sandbox-init: --bind-mount and --bind-mount-target must be paired, got %d sources and %d targets",
			len(bindMountSrcs), len(bindMountTargets))
	}
	for i := range bindMountSrcs {
		cfg.BindMounts = append(cfg.BindMounts, BindMountSpec{
			Source: bindMountSrcs[i],
			Target: bindMountTargets[i],
		})
	}
	// --groups：逗号分隔的补充组 GID 列表（含主组）。仅由 re-exec 的 child
	// （sandbox-init --child）携带，初始调用不含；Run 会在 root 阶段自行解析
	// 目标用户的补充组填入 cfg.Groups。
	if *groups != "" {
		for _, s := range strings.Split(*groups, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			g, err := strconv.Atoi(s)
			if err != nil || g <= 0 {
				log.Fatalf("sandbox-init: invalid --groups entry %q", s)
			}
			cfg.Groups = append(cfg.Groups, g)
		}
	}
	_ = child // child 标记仅由 Run 内部处理，外部不可用

	if err := Run(cfg); err != nil {
		log.Fatalf("sandbox-init: %v", err)
	}
}
