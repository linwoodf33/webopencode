package auth

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ADConfig 保存 AD(LDAPS) 连接与目录查询相关配置。
type ADConfig struct {
	// Server LDAPS 服务器地址，格式 host:port，例如 ldaps.example.com:636。
	Server string `yaml:"server"`
	// ManagerDN manager 账号的 DN，例如 MANAGER@DOMAIN。
	ManagerDN string `yaml:"manager_dn"`
	// ManagerPassword manager 账号密码。
	ManagerPassword string `yaml:"manager_password"`
	// SearchDN 目录搜索基准 DN。
	SearchDN string `yaml:"search_dn"`
	// TLSInsecure 是否跳过 TLS 证书校验（默认 true）。
	TLSInsecure bool `yaml:"tls_insecure"`
	// Timeout AD 连接/操作超时（秒）。0 或负数走默认 5 秒。
	Timeout int `yaml:"timeout"`
}

// OpencodeConfig 控制登录后是否直接进入 opencode TUI（而非 bash），
// 以及 opencode 二进制与配置同步的来源。
type OpencodeConfig struct {
	// Enabled 是否启用 opencode 模式。false 时退回 bash 模式。
	Enabled bool `yaml:"enabled"`
	// Path opencode 可执行文件绝对路径。
	Path string `yaml:"path"`
	// SyncFrom opencode 共享配置源目录（含 skills/ 与 opencode.jsonc）。
	SyncFrom string `yaml:"sync_from"`
	// Overwrite 同步时是否覆盖目标用户已存在的 opencode.jsonc。
	// true：全量覆盖；false：opencode.jsonc 仅在目标不存在时复制（skills 始终覆盖）。
	// 用 *bool 以区分「未设置」（缺省 true）与显式 false。
	Overwrite *bool `yaml:"overwrite"`
}

// OverwriteEnabled 返回 overwrite 是否生效（缺省 true）。
func (o *OpencodeConfig) OverwriteEnabled() bool {
	if o == nil || o.Overwrite == nil {
		return true
	}
	return *o.Overwrite
}

// SandboxConfig 控制是否启用 Linux 用户命名空间沙箱，以及沙箱的资源配置。
type SandboxConfig struct {
	// Enabled 是否启用沙箱。false 时退回现有的「sudo -n -u <user> -i bash」路径。
	Enabled bool `yaml:"enabled"`
	// InitPath sandbox-init setuid-root 二进制绝对路径。
	InitPath string `yaml:"init_path"`
	// RootfsPath base rootfs 绝对路径（作为新根只读 bind）。
	RootfsPath string `yaml:"rootfs_path"`
	// MemoryMax cgroup memory.max（如 "512M"），空则不限制。
	MemoryMax string `yaml:"memory_max"`
	// CPUQuota cgroup cpu.max（如 "50000 100000"），空则不限制。
	CPUQuota string `yaml:"cpu_quota"`
	// PidsMax cgroup pids.max，0 不限制。
	PidsMax int `yaml:"pids_max"`
	// TmpSize /tmp tmpfs 大小（如 "256M"）。
	TmpSize string `yaml:"tmp_size"`
	// Network 网络模式：none|loopback|full。
	Network string `yaml:"network"`
	// Seccomp 是否安装 seccomp 黑名单过滤器（缺省 true）。
	Seccomp *bool `yaml:"seccomp"`
	// NoFileLimit RLIMIT_NOFILE。
	NoFileLimit int `yaml:"nofile_limit"`
	// CoreDump 是否允许 core dump（false 时 RLIMIT_CORE=0）。
	CoreDump bool `yaml:"core_dump"`
	// MaxFsize RLIMIT_FSIZE（如 "1G"）。
	MaxFsize string `yaml:"max_fsize"`
	// ProcHidepid 是否以 hidepid=2 挂载 /proc（缺省 true）。
	ProcHidepid *bool `yaml:"proc_hidepid"`
	// DevList 需要 mknod 的设备类型列表（预留，默认最小集）。
	DevList []string `yaml:"dev_list"`
	// CgroupRoot cgroup v2 根目录。
	CgroupRoot string `yaml:"cgroup_root"`
	// BindHosts 是否把宿主机 /etc/hosts 映射到沙箱内 /etc/hosts。
	BindHosts bool `yaml:"bind_hosts"`
	// BindMounts 宿主路径 bind 到沙箱路径的列表（如 NFS 挂载点）。
	BindMounts []BindMount `yaml:"bind_mounts"`
}

// BindMount 表示一个"宿主路径 bind 到沙箱路径"的挂载。
type BindMount struct {
	Source string `yaml:"source"` // 宿主绝对路径（可含已挂载的 NFS）
	Target string `yaml:"target"` // 沙箱内绝对路径
}

// UnmarshalYAML 兼容两种写法（向后兼容）：
//   - 标量字符串："foo" 等价于 {source: "foo", target: "foo"}（source 与 target 相同）
//   - 映射对象：{source: "...", target: "..."}（原有形式）
//
// 空字符串路径或其它节点类型（如数字/序列）解析失败并返回 error。
func (m *BindMount) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if s == "" {
			return errors.New("bind mount path must not be empty")
		}
		m.Source = s
		m.Target = s
		return nil
	case yaml.MappingNode:
		type raw BindMount
		var r raw
		if err := value.Decode(&r); err != nil {
			return err
		}
		*m = BindMount(r)
		return nil
	default:
		return errors.New("bind mount must be a string or a {source,target} map")
	}
}

// SeccompEnabled 返回 seccomp 是否生效（缺省 true）。
func (s *SandboxConfig) SeccompEnabled() bool {
	if s == nil || s.Seccomp == nil {
		return true
	}
	return *s.Seccomp
}

// ProcHidepidEnabled 返回 /proc hidepid 是否生效（缺省 true）。
func (s *SandboxConfig) ProcHidepidEnabled() bool {
	if s == nil || s.ProcHidepid == nil {
		return true
	}
	return *s.ProcHidepid
}

// NetworkMode 返回规范化的网络模式字符串（小写），非法时返回空串。
func (s *SandboxConfig) NetworkMode() string {
	if s == nil {
		return ""
	}
	switch s.Network {
	case "none", "loopback", "full":
		return s.Network
	default:
		return ""
	}
}

// DefaultSandboxInitPath 默认 sandbox-init 部署路径。
const DefaultSandboxInitPath = "/opt/webshell_sandbox/sandbox-init"

// DefaultSandboxRootfs 默认 base rootfs 路径。
const DefaultSandboxRootfs = "/opt/runner/rootfs/base"

// DefaultSandboxCgroupRoot 默认 cgroup v2 根目录。
const DefaultSandboxCgroupRoot = "/sys/fs/cgroup/webshell_sandbox"

// DefaultOpencodePath 默认 opencode 可执行文件路径。
const DefaultOpencodePath = "/opt/opencode/bin/opencode"

// DefaultOpencodeSyncFrom 默认 opencode 共享配置源目录。
const DefaultOpencodeSyncFrom = "/opt/opencode-config"

// Config 保存 AD 认证与 token 的全部配置。
//
// 唯一配置源为 config.yaml（LoadConfig 读取），不再从环境变量注入。
type Config struct {
	// AD AD(LDAPS) 连接与目录查询配置。
	AD ADConfig `yaml:"ad"`
	// RequirePassword 是否要求用户输入 AD 密码进行强认证。
	// true：manager 搜索取 DN 后用「用户 DN+密码」bind 校验；
	// false：退回目录查询模式（只查目录存在，不校验用户密码）。
	RequirePassword bool `yaml:"require_password"`
	// Listen 服务监听地址与端口，格式 host:port（如 "0.0.0.0:8080" 或 ":8080"）。
	// 为空时走默认 ":8080"。
	Listen string `yaml:"listen"`
	// TokenTTL 一次性 token 有效期（秒）。0 或负数走默认 5 分钟。
	TokenTTL int `yaml:"token_ttl"`
	// Opencode 登录后直接进入 opencode TUI 的配置（enabled=false 退回 bash）。
	Opencode OpencodeConfig `yaml:"opencode"`
	// Sandbox Linux 用户命名空间沙箱配置（enabled=false 退回传统 sudo 模式）。
	Sandbox SandboxConfig `yaml:"sandbox"`
	// ADTimeout AD 连接/操作超时（派生，不参与 yaml）。
	ADTimeout time.Duration
	// TokenTTLd token 有效期（派生，不参与 yaml）。
	TokenTTLd time.Duration
}

// DefaultADTimeout 默认 AD 连接/操作超时（5 秒）。
const DefaultADTimeout = 5 * time.Second

// DefaultTokenTTL 默认一次性 token 有效期（5 分钟）。
const DefaultTokenTTL = 5 * time.Minute

// DefaultListen 默认服务监听地址（":8080"，监听所有网卡的 8080 端口）。
const DefaultListen = ":8080"

// ExampleConfig 示例配置内容（供参考/生成 config.example.yaml）。
const ExampleConfig = `# Web Shell 配置文件（唯一配置源）
# 服务监听地址与端口（host:port），留空走默认 ":8080"
# 注：若显式设置环境变量 PORT，则优先于此处
listen: "0.0.0.0:8080"

ad:
  # LDAPS 服务器地址 host:port
  server: "ldaps.example.com:636"
  # manager 账号 DN（用于目录查询/搜索用户 DN）
  manager_dn: "MANAGER@DOMAIN"
  manager_password: "CHANGE_ME"
  # 目录搜索基准 DN
  search_dn: "DC=example,DC=com"
  # 是否跳过 TLS 证书校验（默认 true）
  tls_insecure: true
  # AD 连接/操作超时（秒），0 或负数走默认 5
  timeout: 5

# 是否要求用户输入 AD 密码进行强认证
# true：用户 DN + 密码 bind；false：仅目录查询
require_password: true

# 一次性 token 有效期（秒），0 或负数走默认 300
token_ttl: 300

# opencode 模式：登录后是否直接进入 opencode TUI（而非 bash）
# enabled: true 进入 opencode；false（缺省）退回 bash
# path: opencode 可执行文件绝对路径（缺省给默认值）
# sync_from: opencode 共享配置源目录（含 skills/ 与 opencode.jsonc），缺省给默认值
# overwrite: true 全量覆盖目标 opencode.jsonc；false 仅在目标不存在时复制
opencode:
  enabled: false
  path: "/opt/opencode/bin/opencode"
  sync_from: "/opt/opencode-config"
  overwrite: true

# Linux 用户命名空间沙箱（setuid-root 的 sandbox-init 作为唯一提权入口）
# enabled: true 启用沙箱；false（缺省）退回传统 sudo 切用户模式
# init_path: sandbox-init 二进制绝对路径（缺省给默认值）
# rootfs_path: base rootfs 绝对路径（作为只读新根 bind，缺省给默认值）
# network: none|loopback|full（缺省 none，即无网络）
# memory_max: cgroup memory.max（如 "512M"），空则不限制
# cpu_quota: cgroup cpu.max（如 "50000 100000"），空则不限制
# pids_max: cgroup pids.max，0 不限制
# tmp_size: /tmp tmpfs 大小（缺省 256M）
# nofile_limit: RLIMIT_NOFILE（缺省 4096）
# core_dump: true 允许 core dump；false（缺省）RLIMIT_CORE=0
# max_fsize: RLIMIT_FSIZE（缺省 1G）
# seccomp: true（缺省）安装 seccomp 黑名单；false 关闭
# proc_hidepid: true（缺省）以 hidepid=2 挂载 /proc；false 关闭
# cgroup_root: cgroup v2 根目录（缺省给默认值）
sandbox:
  enabled: false
  init_path: "/opt/webshell_sandbox/sandbox-init"
  rootfs_path: "/opt/runner/rootfs/base"
  network: "none"
  tmp_size: "256M"
  nofile_limit: 4096
  core_dump: false
  max_fsize: "1G"
  seccomp: true
  proc_hidepid: true
  cgroup_root: "/sys/fs/cgroup/webshell_sandbox"
`

// LoadConfig 从指定的 config.yaml 文件加载配置。
// 文件不存在时返回 os.ErrNotExist（可被 errors.Is 判定）。
// fail-fast：server / manager_dn / manager_password / search_dn 任一为空均报错。
// timeout / token_ttl 为 0 或负数时使用默认值（5 秒 / 5 分钟）。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // 文件不存在时错误即 os.ErrNotExist
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// fail-fast 校验必填项。
	if cfg.AD.Server == "" {
		return nil, errors.New("missing required config ad.server")
	}
	if cfg.AD.ManagerDN == "" {
		return nil, errors.New("missing required config ad.manager_dn")
	}
	if cfg.AD.ManagerPassword == "" {
		return nil, errors.New("missing required config ad.manager_password")
	}
	if cfg.AD.SearchDN == "" {
		return nil, errors.New("missing required config ad.search_dn")
	}

	// 超时/TTL：0 或负数走默认。
	timeout := time.Duration(cfg.AD.Timeout) * time.Second
	if cfg.AD.Timeout <= 0 {
		timeout = DefaultADTimeout
	}
	cfg.ADTimeout = timeout

	ttl := time.Duration(cfg.TokenTTL) * time.Second
	if cfg.TokenTTL <= 0 {
		ttl = DefaultTokenTTL
	}
	cfg.TokenTTLd = ttl

	// 监听地址：为空走默认 ":8080"。
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}

	// opencode 配置默认值：Path / SyncFrom 缺省给默认路径；Overwrite 缺省 true。
	if cfg.Opencode.Path == "" {
		cfg.Opencode.Path = DefaultOpencodePath
	}
	if cfg.Opencode.SyncFrom == "" {
		cfg.Opencode.SyncFrom = DefaultOpencodeSyncFrom
	}
	// Overwrite 缺省 true：未显式设置（nil）时视为 true。显式 false 则保留用户语义。
	// （*bool 在 LoadConfig 中不做改写，由 OverwriteEnabled() 统一解释。）

	// sandbox 配置默认值。
	if cfg.Sandbox.InitPath == "" {
		cfg.Sandbox.InitPath = DefaultSandboxInitPath
	}
	if cfg.Sandbox.RootfsPath == "" {
		cfg.Sandbox.RootfsPath = DefaultSandboxRootfs
	}
	if cfg.Sandbox.TmpSize == "" {
		cfg.Sandbox.TmpSize = "256M"
	}
	if cfg.Sandbox.MaxFsize == "" {
		cfg.Sandbox.MaxFsize = "1G"
	}
	if cfg.Sandbox.NoFileLimit <= 0 {
		cfg.Sandbox.NoFileLimit = 4096
	}
	if cfg.Sandbox.CgroupRoot == "" {
		cfg.Sandbox.CgroupRoot = DefaultSandboxCgroupRoot
	}
	// Network 默认 none；Seccomp/ProcHidepid 默认 true。
	if cfg.Sandbox.Network == "" {
		cfg.Sandbox.Network = "none"
	}
	// 网络模式校验（fail-fast）：仅当启用沙箱时才需要。
	if cfg.Sandbox.Enabled {
		if cfg.Sandbox.NetworkMode() == "" {
			return nil, fmt.Errorf("invalid sandbox.network %q (must be none|loopback|full)", cfg.Sandbox.Network)
		}
		if cfg.Sandbox.InitPath == "" {
			return nil, errors.New("missing required config sandbox.init_path")
		}
		if cfg.Sandbox.RootfsPath == "" {
			return nil, errors.New("missing required config sandbox.rootfs_path")
		}
	}

	return cfg, nil
}
