package auth

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
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
	// AuthTimeout api_key 补全弹窗的等待超时（秒）。0 或负数走默认 60 秒。
	AuthTimeout int `yaml:"auth_timeout"`
	// AuthFile 用户 api_key 存储文件名（位于用户家目录）。空走默认 ".auth.json"。
	AuthFile string `yaml:"auth_file"`
}

// DefaultAuthTimeout opencode api_key 补全弹窗默认等待超时（60 秒）。
const DefaultAuthTimeout = 60

// OverwriteEnabled 返回 overwrite 是否生效（缺省 true）。
func (o *OpencodeConfig) OverwriteEnabled() bool {
	if o == nil || o.Overwrite == nil {
		return true
	}
	return *o.Overwrite
}

// CodexConfig 控制登录后是否直接进入 codex TUI（而非 bash），
// 以及 codex 二进制与配置同步的来源。字段与 OpencodeConfig 平行。
type CodexConfig struct {
	// Enabled 是否启用 codex 模式。false 时退回 bash 模式。
	Enabled bool `yaml:"enabled"`
	// Path codex 可执行文件绝对路径。
	Path string `yaml:"path"`
	// SyncFrom codex 共享配置源目录（同步到用户 ~/.codex，含 config.toml）。
	SyncFrom string `yaml:"sync_from"`
	// Overwrite 同步语义（与 OpencodeConfig 一致，保留字段；但 codex 模式下
	// 以 ~/.codex/config.toml 存在性标记为主，overwrite 影响甚微）。
	// 用 *bool 以区分「未设置」（缺省 true）与显式 false。
	Overwrite *bool `yaml:"overwrite"`
	// AuthTimeout api_key 补全弹窗的等待超时（秒）。0 或负数走默认 60 秒。
	AuthTimeout int `yaml:"auth_timeout"`
	// AuthFile 用户 api_key 存储文件名（位于用户家目录）。空走默认 ".auth.json"。
	AuthFile string `yaml:"auth_file"`
}

// DefaultCodexPath 默认 codex 可执行文件路径。
const DefaultCodexPath = "/share/apps/.codex-standalone/codex"

// DefaultCodexSyncFrom 默认 codex 共享配置源目录。
const DefaultCodexSyncFrom = "/share/apps/.codex-config"

// OverwriteEnabled 返回 overwrite 是否生效（缺省 true）。
func (c *CodexConfig) OverwriteEnabled() bool {
	if c == nil || c.Overwrite == nil {
		return true
	}
	return *c.Overwrite
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
	// BindResolv 是否把宿主机 /etc/resolv.conf 映射到沙箱内 /etc/resolv.conf
	//（解决沙箱内域名无法解析；需 network: full 时 127.0.0.53 才可达）。
	BindResolv bool `yaml:"bind_resolv"`
	// UsernsInitPath userns-init 可执行文件绝对路径；空则由 sandbox-init 所在目录推导。
	UsernsInitPath string `yaml:"userns_init_path"`
	// HTTPProxy http 代理地址；非空时注入沙箱环境变量 http_proxy/HTTP_PROXY。
	HTTPProxy string `yaml:"http_proxy"`
	// HTTPSProxy https 代理地址；非空时注入沙箱环境变量 https_proxy/HTTPS_PROXY。
	HTTPSProxy string `yaml:"https_proxy"`
	// NoProxy 不使用代理的主机列表；非空时注入沙箱环境变量 no_proxy/NO_PROXY。
	NoProxy string `yaml:"no_proxy"`
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
const DefaultSandboxInitPath = "/opt/webshell_sandbox_new/sandbox-init"

// DefaultSandboxRootfs 默认 base rootfs 路径。
const DefaultSandboxRootfs = "/opt/runner/rootfs/base"

// DefaultSandboxCgroupRoot 默认 cgroup v2 根目录。
const DefaultSandboxCgroupRoot = "/sys/fs/cgroup/webshell_sandbox"

// DefaultOpencodePath 默认 opencode 可执行文件路径。
const DefaultOpencodePath = "/opt/opencode/bin/opencode"

// DefaultOpencodeSyncFrom 默认 opencode 共享配置源目录。
const DefaultOpencodeSyncFrom = "/opt/opencode-config"

// AuditConfig 审计录制配置（DESIGN_AUDIT §10）。
//
// Enabled / RecordInput / UserCanViewSelf 用 *bool 以区分「未设置」（缺省 true）
// 与显式 false，与 OpencodeConfig.Overwrite 等既有约定一致。
type AuditConfig struct {
	// Enabled 是否启用录制（false 时埋点为空操作，零开销）；缺省 true。
	Enabled *bool `yaml:"enabled"`
	// Dir 录制根目录；空走默认 DefaultAuditDir。
	Dir string `yaml:"dir"`
	// RecordInput 是否录制输入事件（"i"）；缺省 true。
	RecordInput *bool `yaml:"record_input"`
	// PasswordInput ECHO 关闭时输入处理：mask（默认）|omit|plaintext。
	PasswordInput string `yaml:"password_input"`
	// PasswordMaskWhen 脱敏判据：echo_verify（默认，EVW 输入间边界窗口）|
	// echo_verify_tui（EVW + alt-screen TUI 门）|echo_off_canonical（遗留）|echo_off（严格）。
	PasswordMaskWhen string `yaml:"password_mask_when"`
	// EchoVerifyHangMs EVW hang 兜底时长（毫秒）：pending 的观察窗口未被下一个
	// 输入块/边界事件关闭时，超过此时长一律按 hang 策略兜底（默认掩码）。<=0 走默认 2000。
	EchoVerifyHangMs int `yaml:"echo_verify_hang_ms"`
	// EchoVerifyHangPolicy hang 兜底裁决策略：mask（默认）|reached_plain。
	EchoVerifyHangPolicy string `yaml:"echo_verify_hang_policy"`
	// EchoVerifyStrictBatch echo=on 且整块含换行时一律掩码（严格整块，默认 false）。
	EchoVerifyStrictBatch bool `yaml:"echo_verify_strict_batch"`
	// EchoVerifyLatencyMs 旧键（已废弃，兼容别名）：按 hang 兜底语义读取。
	EchoVerifyLatencyMs int `yaml:"echo_verify_latency_ms"`
	// EchoVerifyTUI 是否启用 alt-screen TUI 门（可被 echo_verify_tui 判据隐式开启）。
	EchoVerifyTUI bool `yaml:"echo_verify_tui"`
	// MaxSessionBytes 单会话 cast 上限（如 "100M"）；空走默认。
	MaxSessionBytes string `yaml:"max_session_bytes"`
	// MaxTotalBytes 总量配额（如 "20G"）；空走默认。
	MaxTotalBytes string `yaml:"max_total_bytes"`
	// RetentionDays 保留天数（按 ended_at 判定）。
	// 用 *int 以区分「未设置」（缺省 90 天）与显式 0（不自动清理），
	// 与 Enabled/RecordInput/UserCanViewSelf 的 *bool 约定一致。
	RetentionDays *int `yaml:"retention_days"`
	// CleanupInterval 清扫间隔（如 "1h"）；空走默认。
	CleanupInterval string `yaml:"cleanup_interval"`
	// FailClosed recorder 打开失败时是否拒绝建会话；默认 false（fail-open）。
	FailClosed bool `yaml:"fail_closed"`
	// TrustProxy 是否信任 X-Forwarded-For（防伪造，默认关闭）。
	TrustProxy bool `yaml:"trust_proxy"`
	// UserCanViewSelf 普通用户是否可回放本人录屏；缺省 true。
	UserCanViewSelf *bool `yaml:"user_can_view_self"`
	// Admins 审计员用户名白名单。
	Admins []string `yaml:"admins"`
}

// EnabledValue 返回审计是否启用（缺省 true）。
func (a *AuditConfig) EnabledValue() bool {
	if a == nil || a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// RecordInputEnabled 返回是否录制输入（缺省 true）。
func (a *AuditConfig) RecordInputEnabled() bool {
	if a == nil || a.RecordInput == nil {
		return true
	}
	return *a.RecordInput
}

// UserCanViewSelfEnabled 返回普通用户是否可查看本人录屏（缺省 true）。
func (a *AuditConfig) UserCanViewSelfEnabled() bool {
	if a == nil || a.UserCanViewSelf == nil {
		return true
	}
	return *a.UserCanViewSelf
}

// 审计默认值（DESIGN_AUDIT §10 / §15）。
const (
	// DefaultAuditDir 默认录制根目录。
	DefaultAuditDir = "/var/log/webshell/audit"
	// DefaultAuditMaxSessionBytes 单会话 cast 默认上限（100MB）。
	DefaultAuditMaxSessionBytes int64 = 100 << 20
	// DefaultAuditMaxTotalBytes 总量默认配额（20GB）。
	DefaultAuditMaxTotalBytes int64 = 20 << 30
	// DefaultAuditRetention 默认保留期（90 天）。
	DefaultAuditRetention = 90 * 24 * time.Hour
	// DefaultAuditCleanupInterval 默认清扫间隔（1h）。
	DefaultAuditCleanupInterval = time.Hour
	// DefaultAuditPasswordInput ECHO 关闭时默认输入处理策略。
	DefaultAuditPasswordInput = "mask"
	// DefaultAuditPasswordMaskWhen 默认脱敏判据（EVW 输入间边界窗口）。
	DefaultAuditPasswordMaskWhen = "echo_verify"
	// DefaultAuditEchoVerifyHangMs EVW hang 兜底默认时长（毫秒）。
	DefaultAuditEchoVerifyHangMs = 2000
	// DefaultAuditEchoVerifyHangPolicy hang 兜底默认策略（一律掩码）。
	DefaultAuditEchoVerifyHangPolicy = "mask"
)

// parseSize 解析带单位的大小字符串（如 "100M"、"20G"、"1_000K"）为字节数。
//
// 支持 K/M/G/T（大小写不敏感，可带尾随 "B"，如 "100MB"），1024 进制；
// 数字与单位间的下划线被忽略（如 "1_000M"）。空串返回 def；非法返回 error。
func parseSize(s string, def int64) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	s = strings.ReplaceAll(s, "_", "")

	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	numPart := s[:i]
	if numPart == "" {
		return 0, fmt.Errorf("missing number in size %q", s)
	}
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number in size %q: %w", s, err)
	}

	unit := strings.ToUpper(strings.TrimSpace(s[i:]))
	unit = strings.TrimSuffix(unit, "B") // "MB" -> "M"，"B" -> ""
	var mult int64
	switch unit {
	case "":
		mult = 1
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown unit %q in size %q", s[i:], s)
	}
	// 溢出检查：n*mult 超过 int64 上限会回绕为负（后续会被误解释为「不限制」）。
	if mult != 0 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return n * mult, nil
}

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
	// WebUsersFile 登录白名单文件绝对路径（独立的文本文件，一行一用户名）。
	// 空 = 未配置：默认 fail-closed（全拒）。文件热更新：改文件后下一次登录即生效。
	// 三态语义见 WebUsersFailOpen 注释。
	WebUsersFile string `yaml:"web_users_file"`
	// WebUsersFailOpen 白名单逃生阀。true 时无条件全放行（慎用）；缺省 false。
	// 三态语义：
	//   fail_open=true                 → 恒放行；
	//   未配置(path=="") && !fail_open → 恒拒绝（fail-closed）；
	//   已配置但文件缺失/为空/全注释   → 全拒（fail-closed）。
	WebUsersFailOpen bool `yaml:"web_users_fail_open"`
	// Listen 服务监听地址与端口，格式 host:port（如 "0.0.0.0:8080" 或 ":8080"）。
	// 为空时走默认 ":8080"。
	Listen string `yaml:"listen"`
	// SessionTTLHours 新增长期会话的默认时长（小时）。0 或负数走默认 24。
	SessionTTLHours float64 `yaml:"session_ttl_hours"`
	// MaxSessionTTLHours 单次输入最大时长（小时，新建与延期共用）。0 或负数走默认 168。
	MaxSessionTTLHours float64 `yaml:"max_session_ttl_hours"`
	// MaxLongSessions 每用户长期会话数量上限。0 或负数走默认 3。
	MaxLongSessions int `yaml:"max_long_sessions"`
	// MaxTotalSessions 每用户会话总数上限（长+短）。0 或负数走默认 10。
	MaxTotalSessions int `yaml:"max_total_sessions"`
	// MgmtTTLHours mgmt_token 有效期（小时）。0 或负数走默认 8。
	MgmtTTLHours float64 `yaml:"mgmt_ttl_hours"`
	// Opencode 登录后直接进入 opencode TUI 的配置（enabled=false 退回 bash）。
	Opencode OpencodeConfig `yaml:"opencode"`
	// Codex 登录后直接进入 codex TUI 的配置（enabled=false 退回 bash）。
	Codex CodexConfig `yaml:"codex"`
	// Sandbox Linux 用户命名空间沙箱配置（enabled=false 退回传统 sudo 模式）。
	Sandbox SandboxConfig `yaml:"sandbox"`
	// Audit 审计录制配置（DESIGN_AUDIT §10）。
	Audit AuditConfig `yaml:"audit"`
	// ADTimeout AD 连接/操作超时（派生，不参与 yaml）。
	ADTimeout time.Duration
	// SessionTTLd 默认长期会话时长（派生，不参与 yaml）。
	// TODO(P2): remove if unused by production —— 生产代码未消费，仅 config_test
	// 断言；保留因 DESIGN_SESSIONS.md 仍将其列为 Config 结构成员。
	SessionTTLd time.Duration
	// MaxAttachTTL 单次最大时长/增量（派生，不参与 yaml）。
	// TODO(P2): remove if unused by production —— 同上，生产代码未消费。
	MaxAttachTTL time.Duration
	// MgmtTTLd mgmt_token 有效期（派生，不参与 yaml）。
	MgmtTTLd time.Duration
	// MaxLongN 长期会话数量上限（派生，不参与 yaml）。
	MaxLongN int
	// MaxTotalN 会话总数上限（派生，不参与 yaml）。
	MaxTotalN int

	// AuditEnabled 审计是否启用（派生，不参与 yaml）。
	AuditEnabled bool
	// AuditDir 录制根目录（派生，不参与 yaml）。
	AuditDir string
	// AuditRecordIn 是否录制输入事件（派生，不参与 yaml）。
	AuditRecordIn bool
	// AuditPasswordMode ECHO 关闭时输入处理：mask|omit|plaintext（派生，不参与 yaml）。
	AuditPasswordMode string
	// AuditPasswordMaskWhen 脱敏判据：echo_verify|echo_verify_tui|echo_off_canonical|echo_off
	//（派生，不参与 yaml）。
	AuditPasswordMaskWhen string
	// AuditEchoVerifyTimeout EVW hang 兜底时长（派生，不参与 yaml）。
	AuditEchoVerifyTimeout time.Duration
	// AuditEchoVerifyHangPolicy hang 兜底裁决策略（派生，不参与 yaml）：mask|reached_plain。
	AuditEchoVerifyHangPolicy string
	// AuditEchoVerifyStrictBatch echo=on 整块含换行一律掩码（派生，不参与 yaml）。
	AuditEchoVerifyStrictBatch bool
	// AuditEchoVerifyTUI 是否启用 alt-screen TUI 门（派生，不参与 yaml）。
	AuditEchoVerifyTUI bool
	// AuditMaxBytes 单会话 cast 字节上限（派生，不参与 yaml）。
	AuditMaxBytes int64
	// AuditTotalBytes 总量配额字节数（派生，不参与 yaml）。
	AuditTotalBytes int64
	// AuditRetention 保留期（派生，不参与 yaml；0 表示不自动清理）。
	AuditRetention time.Duration
	// AuditCleanupIvl 清扫间隔（派生，不参与 yaml）。
	AuditCleanupIvl time.Duration
	// AuditFailClosed recorder 打开失败时是否拒绝建会话（派生，不参与 yaml）。
	AuditFailClosed bool
	// AuditTrustProxy 是否信任 X-Forwarded-For（派生，不参与 yaml）。
	AuditTrustProxy bool
	// AuditUserSelf 普通用户是否可回放本人录屏（派生，不参与 yaml）。
	AuditUserSelf bool
	// AuditAdmins 审计员白名单（用户名小写归一，派生，不参与 yaml）。
	AuditAdmins map[string]bool
}

// IsAuditor 判断用户名是否为审计员（白名单，大小写不敏感）。
func (c *Config) IsAuditor(u string) bool {
	if c == nil || c.AuditAdmins == nil {
		return false
	}
	return c.AuditAdmins[strings.ToLower(u)]
}

// DefaultADTimeout 默认 AD 连接/操作超时（5 秒）。
const DefaultADTimeout = 5 * time.Second

// 会话/凭证相关默认值。
const (
	// DefaultSessionTTLHours 新增长期会话默认时长（小时）。
	DefaultSessionTTLHours = 24
	// DefaultMaxSessionTTLHours 单次最大时长（小时）。
	DefaultMaxSessionTTLHours = 168
	// DefaultMaxLongSessions 每用户长期会话数量上限。
	DefaultMaxLongSessions = 3
	// DefaultMaxTotalSessions 每用户会话总数上限（长+短）。
	DefaultMaxTotalSessions = 10
	// DefaultMgmtTTLHours mgmt_token 默认有效期（小时）。
	DefaultMgmtTTLHours = 8
)

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

# 会话与凭证配置（0 或负数走默认值）
# session_ttl_hours: 新增长期会话的默认时长（小时），默认 24
# max_session_ttl_hours: 单次输入最大时长（小时，新建与延期共用），默认 168
# max_long_sessions: 每用户长期会话数量上限，默认 3
# max_total_sessions: 每用户会话总数上限（长+短），默认 10
# mgmt_ttl_hours: 管理 token（mgmt_token）有效期（小时），默认 8
session_ttl_hours: 24
max_session_ttl_hours: 168
max_long_sessions: 3
max_total_sessions: 10
mgmt_ttl_hours: 8

# 登录白名单（独立配置文件；编辑文件后下一次登录即时生效，无需重启）
# web_users_file: 白名单文件绝对路径；空（缺省）= 未配置，按 fail-closed 全拒
#   文件格式：一行一个用户名；整行以 # 开头为注释；空行忽略；不支持行尾 # 注释
#   用户名校验：^[a-z_][a-z0-9_-]{0,31}$（大小写不敏感，匹配时统一小写）
# web_users_fail_open: 逃生阀；true 时无条件全放行（慎用）；缺省 false
#   三态语义：fail_open=true → 恒放行；未配置 + fail_open=false → 恒拒绝；
#             已配置但文件缺失/为空/全注释 → 全拒（fail-closed）
web_users_file: "/etc/webshell/web_users"
web_users_fail_open: false

# opencode 模式：登录后是否直接进入 opencode TUI（而非 bash）
# enabled: true 进入 opencode；false（缺省）退回 bash
# path: opencode 可执行文件绝对路径（缺省给默认值）
# sync_from: opencode 共享配置源目录（含 skills/ 与 opencode.jsonc），缺省给默认值
#   源含 AGENTS.md：每次启动会话时检查 ~/.config/opencode/AGENTS.md，缺失且源存在才复制
#   （独立于整体同步跳过标记、已存在不覆盖、源缺失静默跳过、与 overwrite 无关）
#   源含 agent/ 目录（策略 A）：源存在且非空、目标 ~/.config/opencode/agent 不存在才整体复制；
#   目标已有则跳过（保留用户自定义 agent）；源目录为空时不创建目标空目录
# overwrite: true 全量覆盖目标 opencode.jsonc；false 仅在目标不存在时复制
opencode:
  enabled: false
  path: "/opt/opencode/bin/opencode"
  sync_from: "/opt/opencode-config"
  overwrite: true

# codex 模式：登录后是否直接进入 codex TUI（而非 bash）
# enabled: true 进入 codex；false（缺省）退回 bash
# path: codex 可执行文件绝对路径（缺省给默认值）
# sync_from: codex 共享配置源目录，同步到用户 ~/.codex（注意不是 ~/.config/codex），缺省给默认值
#   白名单同步：只同步 config.toml 与 skills/，绝不同步任何运行态/敏感文件
#   （*.sqlite/*.jsonl/日志/会话/installation_id/version.json 等）。
#   源含 AGENTS.md：每次启动会话时检查 ~/.codex/AGENTS.md，缺失且源存在才复制
#   （独立于整体同步跳过标记、已存在不覆盖、源缺失静默跳过、与 overwrite 无关）。
#   源含 agents/ 目录（策略 A）：源存在且非空、目标 ~/.codex/agents 不存在才整体复制；
#   目标已有则跳过（保留用户自定义 agent）；源目录为空时不创建目标空目录
# overwrite: true 全量覆盖；false 仅在目标不存在时复制。
#   注意：codex 模式下以 ~/.codex/config.toml 存在性标记为主（已存在即整体跳过），
#   overwrite 影响甚微，字段仅为与 opencode 对齐保留。
# auth_timeout: 启动前 api_key 补全弹窗的等待超时（秒），0 或缺省走默认 60
# auth_file: 用户 api_key 存储文件名（位于用户家目录），缺省 ".auth.json"
#
# 注意：codex 配置（~/.codex/config.toml）中的 api_key 必须用 env_key 引用环境变量
# （如 model_providers.<id>.env_key = "YANFENG_API_KEY"），禁止硬编码 key。Web Shell
# 会读取该引用，若用户 ~/.auth.json 缺失对应 api_key，则在 codex 启动前弹窗让用户补全
# （复用 opencode 的 auth-request/auth-response 机制）。硬编码 key 将无法触发本机制，
# 且会随配置同步泄露给所有用户。
codex:
  enabled: false
  path: "/share/apps/.codex-standalone/codex"
  sync_from: "/share/apps/.codex-config"
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
# bind_hosts: true 把宿主机 /etc/hosts 映射进沙箱（解决沙箱内主机名解析）
# bind_resolv: true 把宿主机 /etc/resolv.conf 映射进沙箱（解决沙箱内域名解析；
#   建议 network: full 时开启，此时沙箱共享宿主网络栈，宿主 DNS（如
#   127.0.0.53 systemd-resolved stub）在沙箱内可达）
# userns_init_path: ""  # userns-init 可执行文件绝对路径；留空自动推导为 sandbox-init 同目录下的 userns-init
# http_proxy: ""    # http 代理（如 "http://proxy.example.com:8080"），非空时注入沙箱环境变量
# https_proxy: ""   # https 代理（如 "http://proxy.example.com:8080"），非空时注入沙箱环境变量
# no_proxy: ""      # 不使用代理的主机（如 "localhost,127.0.0.1"），非空时注入沙箱环境变量
sandbox:
  enabled: false
  init_path: "/opt/webshell_sandbox_new/sandbox-init"
  rootfs_path: "/opt/runner/rootfs/base"
  network: "none"
  tmp_size: "256M"
  nofile_limit: 4096
  core_dump: false
  max_fsize: "1G"
  seccomp: true
  proc_hidepid: true
  cgroup_root: "/sys/fs/cgroup/webshell_sandbox"

# 审计录制（DESIGN_AUDIT §10）
# enabled: 是否启用录制（缺省 true；false 时埋点为空操作，零开销）
# dir: 录制根目录（缺省 "/var/log/webshell/audit"）
# record_input: 是否录制输入事件（缺省 true）；false 仅录 output/resize
# password_input: ECHO 关闭时的输入处理，mask（缺省，等长掩码）| omit（不记）| plaintext（明文，慎用）
# password_mask_when: 脱敏判据：
#   echo_verify（缺省）：EVW 输入间边界窗口——raw 输入先待定，其观察窗口为
#     [输入块到达, 下一个输入块/边界事件)；窗口内观察到回显则明文、否则掩码。
#     窗口迟迟未被关闭时由 hang 兜底（缺省掩码），避免末尾字符被 Enter 后
#     printback 误判明文。echo_verify_tui：同 EVW 且启用 alt-screen TUI 门
#     （进入 alt-screen 后输入直接明文）；echo_off_canonical（遗留，仅
#     ECHO=off && ICANON=on 掩码，raw 不脱敏）；echo_off（严格，ECHO=off 即掩码）
# echo_verify_hang_ms: EVW hang 兜底时长（毫秒，缺省 2000）；窗口未被下一输入/边界
#   关闭时，超过此时长一律按 hang 策略结算
# echo_verify_hang_policy: hang 兜底策略，mask（缺省，一律掩码）| reached_plain（已
#   观测到回显的记明文）
# echo_verify_strict_batch: echo=on 且整块含换行时一律掩码（缺省 false，严格整块）
# echo_verify_latency_ms: 【已废弃】旧键，兼容别名，按 hang 兜底语义读取
# echo_verify_tui: 是否启用 alt-screen TUI 门（缺省 false；echo_verify_tui 判据会隐式开启）
# max_session_bytes: 单会话 cast 上限（缺省 "100M"）
# max_total_bytes: 总量配额，最旧优先清理（缺省 "20G"）
# retention_days: 保留期（按 ended_at 判定，0 表示不自动清理，缺省 90）
# cleanup_interval: 清扫间隔（缺省 "1h"）
# fail_closed: recorder 打开失败时是否拒绝建会话；缺省 false（照常开会话，fail-open）
# trust_proxy: 是否信任 X-Forwarded-For（防伪造，缺省 false）
# user_can_view_self: 普通用户是否可回放本人录屏（缺省 true）
# admins: 审计员用户名白名单
audit:
  enabled: true
  dir: "/var/log/webshell/audit"
  record_input: true
  password_input: mask
  password_mask_when: echo_verify
  echo_verify_hang_ms: 2000
  echo_verify_hang_policy: mask
  echo_verify_strict_batch: false
  echo_verify_tui: false
  max_session_bytes: "100M"
  max_total_bytes: "20G"
  retention_days: 90
  cleanup_interval: "1h"
  fail_closed: false
  trust_proxy: false
  user_can_view_self: true
  admins: ["alice", "auditor"]
`

// LoadConfig 从指定的 config.yaml 文件加载配置。
// 文件不存在时返回 os.ErrNotExist（可被 errors.Is 判定）。
// fail-fast：server / manager_dn / manager_password / search_dn 任一为空均报错。
// timeout 为 0 或负数时使用默认值（5 秒）；会话/凭证配置同理走默认值。
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

	// 会话/凭证配置：0 或负数走默认值，并派生 duration / 计数。
	if cfg.SessionTTLHours <= 0 {
		cfg.SessionTTLHours = DefaultSessionTTLHours
	}
	if cfg.MaxSessionTTLHours <= 0 {
		cfg.MaxSessionTTLHours = DefaultMaxSessionTTLHours
	}
	if cfg.MaxLongSessions <= 0 {
		cfg.MaxLongSessions = DefaultMaxLongSessions
	}
	if cfg.MaxTotalSessions <= 0 {
		cfg.MaxTotalSessions = DefaultMaxTotalSessions
	}
	if cfg.MgmtTTLHours <= 0 {
		cfg.MgmtTTLHours = DefaultMgmtTTLHours
	}
	cfg.SessionTTLd = time.Duration(cfg.SessionTTLHours * float64(time.Hour))
	cfg.MaxAttachTTL = time.Duration(cfg.MaxSessionTTLHours * float64(time.Hour))
	cfg.MgmtTTLd = time.Duration(cfg.MgmtTTLHours * float64(time.Hour))
	cfg.MaxLongN = cfg.MaxLongSessions
	cfg.MaxTotalN = cfg.MaxTotalSessions

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

	// codex 配置默认值：Path / SyncFrom 缺省给默认路径；Overwrite 缺省 true。
	if cfg.Codex.Path == "" {
		cfg.Codex.Path = DefaultCodexPath
	}
	if cfg.Codex.SyncFrom == "" {
		cfg.Codex.SyncFrom = DefaultCodexSyncFrom
	}
	// Overwrite 缺省 true，由 OverwriteEnabled() 统一解释（同 opencode）。

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

	// 登录白名单：web_users_file / web_users_fail_open 无默认值（三态语义由
	// UserWhitelist 运行时解释：未配置=fail-closed，不在此派生）。

	// audit 配置派生与默认（DESIGN_AUDIT §10）。
	cfg.AuditEnabled = cfg.Audit.EnabledValue()
	if cfg.Audit.Dir == "" {
		cfg.Audit.Dir = DefaultAuditDir
	}
	cfg.AuditDir = cfg.Audit.Dir
	cfg.AuditRecordIn = cfg.Audit.RecordInputEnabled()

	if cfg.Audit.PasswordInput == "" {
		cfg.Audit.PasswordInput = DefaultAuditPasswordInput
	}
	cfg.AuditPasswordMode = cfg.Audit.PasswordInput
	if cfg.Audit.PasswordMaskWhen == "" {
		cfg.Audit.PasswordMaskWhen = DefaultAuditPasswordMaskWhen
	}
	cfg.AuditPasswordMaskWhen = cfg.Audit.PasswordMaskWhen

	// EVW hang 兜底时长：优先 echo_verify_hang_ms；其次旧键 echo_verify_latency_ms
	// 兼容映射（并告警建议改名）；均未设置走默认 2000ms。
	if cfg.Audit.EchoVerifyHangMs > 0 {
		cfg.AuditEchoVerifyTimeout = time.Duration(cfg.Audit.EchoVerifyHangMs) * time.Millisecond
	} else if cfg.Audit.EchoVerifyLatencyMs > 0 {
		cfg.AuditEchoVerifyTimeout = time.Duration(cfg.Audit.EchoVerifyLatencyMs) * time.Millisecond
		log.Printf("audit: config key echo_verify_latency_ms is deprecated; use echo_verify_hang_ms (value now treated as EVW hang fallback timeout)")
	} else {
		cfg.AuditEchoVerifyTimeout = DefaultAuditEchoVerifyHangMs * time.Millisecond
	}

	// hang 兜底策略：空走默认 mask。
	if cfg.Audit.EchoVerifyHangPolicy == "" {
		cfg.Audit.EchoVerifyHangPolicy = DefaultAuditEchoVerifyHangPolicy
	}
	cfg.AuditEchoVerifyHangPolicy = cfg.Audit.EchoVerifyHangPolicy
	cfg.AuditEchoVerifyStrictBatch = cfg.Audit.EchoVerifyStrictBatch

	// echo_verify_tui 判据隐式开启 TUI 门。
	cfg.AuditEchoVerifyTUI = cfg.Audit.EchoVerifyTUI || cfg.AuditPasswordMaskWhen == "echo_verify_tui"

	maxSession, err := parseSize(cfg.Audit.MaxSessionBytes, DefaultAuditMaxSessionBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid config audit.max_session_bytes: %w", err)
	}
	cfg.AuditMaxBytes = maxSession
	maxTotal, err := parseSize(cfg.Audit.MaxTotalBytes, DefaultAuditMaxTotalBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid config audit.max_total_bytes: %w", err)
	}
	cfg.AuditTotalBytes = maxTotal

	// retention_days 三态语义：
	//   nil（未设置）→ 默认 DefaultAuditRetention（90 天）
	//   显式 >0     → 对应天数
	//   显式 0      → 0（不自动清理）
	//   显式 <0     → 非法
	if cfg.Audit.RetentionDays != nil {
		if *cfg.Audit.RetentionDays < 0 {
			return nil, fmt.Errorf("invalid config audit.retention_days %d (must be >= 0)", *cfg.Audit.RetentionDays)
		}
		if *cfg.Audit.RetentionDays == 0 {
			cfg.AuditRetention = 0
		} else {
			cfg.AuditRetention = time.Duration(*cfg.Audit.RetentionDays) * 24 * time.Hour
		}
	} else {
		cfg.AuditRetention = DefaultAuditRetention
	}

	// cleanup_interval：空走默认；非法或非正数报错（P2 将用于 time.Ticker）。
	cleanupIvl := DefaultAuditCleanupInterval
	if cfg.Audit.CleanupInterval != "" {
		d, derr := time.ParseDuration(cfg.Audit.CleanupInterval)
		if derr != nil {
			return nil, fmt.Errorf("invalid config audit.cleanup_interval %q: %w", cfg.Audit.CleanupInterval, derr)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid config audit.cleanup_interval %q (must be > 0)", cfg.Audit.CleanupInterval)
		}
		cleanupIvl = d
	}
	cfg.AuditCleanupIvl = cleanupIvl

	cfg.AuditFailClosed = cfg.Audit.FailClosed
	cfg.AuditTrustProxy = cfg.Audit.TrustProxy
	cfg.AuditUserSelf = cfg.Audit.UserCanViewSelfEnabled()

	// 审计员白名单：用户名小写归一（与 IsAuditor 同一口径）。
	cfg.AuditAdmins = make(map[string]bool, len(cfg.Audit.Admins))
	for _, a := range cfg.Audit.Admins {
		if a == "" {
			continue
		}
		cfg.AuditAdmins[strings.ToLower(a)] = true
	}

	// 枚举校验（fail-fast）：仅在启用审计时需要。
	if cfg.AuditEnabled {
		switch cfg.AuditPasswordMode {
		case "mask", "omit", "plaintext":
		default:
			return nil, fmt.Errorf("invalid config audit.password_input %q (must be mask|omit|plaintext)", cfg.Audit.PasswordInput)
		}
		switch cfg.AuditPasswordMaskWhen {
		case "echo_verify", "echo_verify_tui", "echo_off_canonical", "echo_off":
		default:
			return nil, fmt.Errorf("invalid config audit.password_mask_when %q (must be echo_verify|echo_verify_tui|echo_off_canonical|echo_off)", cfg.Audit.PasswordMaskWhen)
		}
		switch cfg.AuditEchoVerifyHangPolicy {
		case "mask", "reached_plain":
		default:
			return nil, fmt.Errorf("invalid config audit.echo_verify_hang_policy %q (must be mask|reached_plain)", cfg.Audit.EchoVerifyHangPolicy)
		}
	}

	return cfg, nil
}
