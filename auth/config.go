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

	return cfg, nil
}
