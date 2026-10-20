package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTempConfig 写一个临时 config.yaml 并返回路径。
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "MANAGER@DOMAIN"
  manager_password: "secret"
  search_dn: "DC=example,DC=com"
  tls_insecure: true
  timeout: 7
require_password: true
session_ttl_hours: 10
max_session_ttl_hours: 100
max_long_sessions: 5
max_total_sessions: 20
mgmt_ttl_hours: 6
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AD.Server != "ldaps.example.com:636" {
		t.Errorf("server = %q", cfg.AD.Server)
	}
	if cfg.AD.ManagerDN != "MANAGER@DOMAIN" {
		t.Errorf("manager_dn = %q", cfg.AD.ManagerDN)
	}
	if cfg.AD.ManagerPassword != "secret" {
		t.Errorf("manager_password = %q", cfg.AD.ManagerPassword)
	}
	if cfg.AD.SearchDN != "DC=example,DC=com" {
		t.Errorf("search_dn = %q", cfg.AD.SearchDN)
	}
	if !cfg.AD.TLSInsecure {
		t.Error("tls_insecure should be true")
	}
	if !cfg.RequirePassword {
		t.Error("require_password should be true")
	}
	if cfg.ADTimeout != 7*time.Second {
		t.Errorf("ADTimeout = %v, want 7s", cfg.ADTimeout)
	}
	if cfg.SessionTTLHours != 10 {
		t.Errorf("SessionTTLHours = %v, want 10", cfg.SessionTTLHours)
	}
	if cfg.SessionTTLd != 10*time.Hour {
		t.Errorf("SessionTTLd = %v, want 10h", cfg.SessionTTLd)
	}
	if cfg.MaxSessionTTLHours != 100 {
		t.Errorf("MaxSessionTTLHours = %v, want 100", cfg.MaxSessionTTLHours)
	}
	if cfg.MaxAttachTTL != 100*time.Hour {
		t.Errorf("MaxAttachTTL = %v, want 100h", cfg.MaxAttachTTL)
	}
	if cfg.MaxLongN != 5 {
		t.Errorf("MaxLongN = %d, want 5", cfg.MaxLongN)
	}
	if cfg.MaxTotalN != 20 {
		t.Errorf("MaxTotalN = %d, want 20", cfg.MaxTotalN)
	}
	if cfg.MgmtTTLHours != 6 {
		t.Errorf("MgmtTTLHours = %v, want 6", cfg.MgmtTTLHours)
	}
	if cfg.MgmtTTLd != 6*time.Hour {
		t.Errorf("MgmtTTLd = %v, want 6h", cfg.MgmtTTLd)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// 未显式设置 timeout / 会话配置 / listen 时走默认值。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ADTimeout != DefaultADTimeout {
		t.Errorf("ADTimeout = %v, want default %v", cfg.ADTimeout, DefaultADTimeout)
	}
	if cfg.SessionTTLd != time.Duration(DefaultSessionTTLHours)*time.Hour {
		t.Errorf("SessionTTLd = %v, want default", cfg.SessionTTLd)
	}
	if cfg.MaxAttachTTL != time.Duration(DefaultMaxSessionTTLHours)*time.Hour {
		t.Errorf("MaxAttachTTL = %v, want default", cfg.MaxAttachTTL)
	}
	if cfg.MgmtTTLd != time.Duration(DefaultMgmtTTLHours)*time.Hour {
		t.Errorf("MgmtTTLd = %v, want default", cfg.MgmtTTLd)
	}
	if cfg.MaxLongN != DefaultMaxLongSessions {
		t.Errorf("MaxLongN = %d, want default %d", cfg.MaxLongN, DefaultMaxLongSessions)
	}
	if cfg.MaxTotalN != DefaultMaxTotalSessions {
		t.Errorf("MaxTotalN = %d, want default %d", cfg.MaxTotalN, DefaultMaxTotalSessions)
	}
	if cfg.RequirePassword {
		t.Error("require_password should default to false")
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q, want default %q", cfg.Listen, DefaultListen)
	}
}

func TestLoadConfigListen(t *testing.T) {
	path := writeTempConfig(t, `
listen: "127.0.0.1:18099"
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:18099" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, "127.0.0.1:18099")
	}
}

func TestLoadConfigMissingServer(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing server, got nil")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	path := writeTempConfig(t, "ad: [unclosed")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid yaml, got nil")
	}
}

func TestLoadConfigOpencodeDefaults(t *testing.T) {
	// 未显式设置 opencode 时：enabled=false，path/sync_from 走默认，overwrite 缺省 true。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Opencode.Enabled {
		t.Error("opencode.enabled should default to false")
	}
	if cfg.Opencode.Path != DefaultOpencodePath {
		t.Errorf("opencode.path = %q, want default %q", cfg.Opencode.Path, DefaultOpencodePath)
	}
	if cfg.Opencode.SyncFrom != DefaultOpencodeSyncFrom {
		t.Errorf("opencode.sync_from = %q, want default %q", cfg.Opencode.SyncFrom, DefaultOpencodeSyncFrom)
	}
	if !cfg.Opencode.OverwriteEnabled() {
		t.Error("opencode.overwrite should default to true")
	}
}

func TestLoadConfigOpencodeExplicit(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
opencode:
  enabled: true
  path: "/opt/opencode"
  sync_from: "/opt/opencode-config"
  overwrite: false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Opencode.Enabled {
		t.Error("opencode.enabled should be true")
	}
	if cfg.Opencode.Path != "/opt/opencode" {
		t.Errorf("opencode.path = %q", cfg.Opencode.Path)
	}
	if cfg.Opencode.SyncFrom != "/opt/opencode-config" {
		t.Errorf("opencode.sync_from = %q", cfg.Opencode.SyncFrom)
	}
	if cfg.Opencode.OverwriteEnabled() {
		t.Error("opencode.overwrite should be false")
	}
}

func TestLoadConfigCodexDefaults(t *testing.T) {
	// 未显式设置 codex 时：enabled=false，path/sync_from 走默认，overwrite 缺省 true。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Codex.Enabled {
		t.Error("codex.enabled should default to false")
	}
	if cfg.Codex.Path != DefaultCodexPath {
		t.Errorf("codex.path = %q, want default %q", cfg.Codex.Path, DefaultCodexPath)
	}
	if cfg.Codex.SyncFrom != DefaultCodexSyncFrom {
		t.Errorf("codex.sync_from = %q, want default %q", cfg.Codex.SyncFrom, DefaultCodexSyncFrom)
	}
	if !cfg.Codex.OverwriteEnabled() {
		t.Error("codex.overwrite should default to true")
	}
}

func TestLoadConfigCodexExplicit(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
codex:
  enabled: true
  path: "/share/apps/.codex-standalone/codex"
  sync_from: "/share/apps/.codex-config"
  overwrite: false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Codex.Enabled {
		t.Error("codex.enabled should be true")
	}
	if cfg.Codex.Path != "/share/apps/.codex-standalone/codex" {
		t.Errorf("codex.path = %q", cfg.Codex.Path)
	}
	if cfg.Codex.SyncFrom != "/share/apps/.codex-config" {
		t.Errorf("codex.sync_from = %q", cfg.Codex.SyncFrom)
	}
	if cfg.Codex.OverwriteEnabled() {
		t.Error("codex.overwrite should be false")
	}
}

func TestLoadConfigCodexEnabledEmptyFallback(t *testing.T) {
	// codex enabled 但 path/sync_from 为空 -> 默认值兜底。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
codex:
  enabled: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Codex.Enabled {
		t.Error("codex.enabled should be true")
	}
	if cfg.Codex.Path != DefaultCodexPath {
		t.Errorf("codex.path = %q, want default %q", cfg.Codex.Path, DefaultCodexPath)
	}
	if cfg.Codex.SyncFrom != DefaultCodexSyncFrom {
		t.Errorf("codex.sync_from = %q, want default %q", cfg.Codex.SyncFrom, DefaultCodexSyncFrom)
	}
}

func TestLoadConfigFileNotFound(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestLoadConfigSandboxBindMounts(t *testing.T) {
	// sandbox 启用时解析 bind_hosts / bind_resolv / bind_mounts 配置。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  bind_hosts: true
  bind_resolv: true
  bind_mounts:
    - source: "/easeshare/SH/Method2"
      target: "/easeshare/SH/Method2"
    - source: "/easeshare/SH/Method1"
      target: "/srv/share/Method1"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Error("sandbox.enabled should be true")
	}
	if !cfg.Sandbox.BindHosts {
		t.Error("sandbox.bind_hosts should be true")
	}
	if !cfg.Sandbox.BindResolv {
		t.Error("sandbox.bind_resolv should be true")
	}
	if len(cfg.Sandbox.BindMounts) != 2 {
		t.Fatalf("sandbox.bind_mounts len = %d, want 2", len(cfg.Sandbox.BindMounts))
	}
	if cfg.Sandbox.BindMounts[0].Source != "/easeshare/SH/Method2" {
		t.Errorf("bind_mounts[0].source = %q", cfg.Sandbox.BindMounts[0].Source)
	}
	if cfg.Sandbox.BindMounts[0].Target != "/easeshare/SH/Method2" {
		t.Errorf("bind_mounts[0].target = %q", cfg.Sandbox.BindMounts[0].Target)
	}
	if cfg.Sandbox.BindMounts[1].Source != "/easeshare/SH/Method1" {
		t.Errorf("bind_mounts[1].source = %q", cfg.Sandbox.BindMounts[1].Source)
	}
	if cfg.Sandbox.BindMounts[1].Target != "/srv/share/Method1" {
		t.Errorf("bind_mounts[1].target = %q", cfg.Sandbox.BindMounts[1].Target)
	}
}

func TestLoadConfigSandboxBindMountsSimpleStrings(t *testing.T) {
	// 字符串形式：source == target == 该路径。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  bind_mounts:
    - "/easeshare/SH/Method2"
    - "/easeshare/SIMU3/Sys_Run"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Sandbox.BindMounts) != 2 {
		t.Fatalf("sandbox.bind_mounts len = %d, want 2", len(cfg.Sandbox.BindMounts))
	}
	if cfg.Sandbox.BindMounts[0].Source != "/easeshare/SH/Method2" {
		t.Errorf("bind_mounts[0].source = %q", cfg.Sandbox.BindMounts[0].Source)
	}
	if cfg.Sandbox.BindMounts[0].Target != "/easeshare/SH/Method2" {
		t.Errorf("bind_mounts[0].target = %q", cfg.Sandbox.BindMounts[0].Target)
	}
	if cfg.Sandbox.BindMounts[1].Source != "/easeshare/SIMU3/Sys_Run" {
		t.Errorf("bind_mounts[1].source = %q", cfg.Sandbox.BindMounts[1].Source)
	}
	if cfg.Sandbox.BindMounts[1].Target != "/easeshare/SIMU3/Sys_Run" {
		t.Errorf("bind_mounts[1].target = %q", cfg.Sandbox.BindMounts[1].Target)
	}
}

func TestLoadConfigSandboxBindMountsObject(t *testing.T) {
	// 对象形式（原有）：source / target 可不同。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  bind_mounts:
    - source: "/easeshare/SH/Method1"
      target: "/srv/share/Method1"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Sandbox.BindMounts) != 1 {
		t.Fatalf("sandbox.bind_mounts len = %d, want 1", len(cfg.Sandbox.BindMounts))
	}
	if cfg.Sandbox.BindMounts[0].Source != "/easeshare/SH/Method1" {
		t.Errorf("bind_mounts[0].source = %q", cfg.Sandbox.BindMounts[0].Source)
	}
	if cfg.Sandbox.BindMounts[0].Target != "/srv/share/Method1" {
		t.Errorf("bind_mounts[0].target = %q", cfg.Sandbox.BindMounts[0].Target)
	}
}

func TestLoadConfigSandboxBindMountsMixed(t *testing.T) {
	// 混合形式：字符串与对象形式可在同列表中混用。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  bind_mounts:
    - "/easeshare/SH/Method2"
    - source: "/easeshare/SH/Method1"
      target: "/srv/share/Method1"
    - "/easeshare/SIMU3/Sys_Run"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Sandbox.BindMounts) != 3 {
		t.Fatalf("sandbox.bind_mounts len = %d, want 3", len(cfg.Sandbox.BindMounts))
	}
	if cfg.Sandbox.BindMounts[0].Source != "/easeshare/SH/Method2" || cfg.Sandbox.BindMounts[0].Target != "/easeshare/SH/Method2" {
		t.Errorf("bind_mounts[0] = {%q, %q}", cfg.Sandbox.BindMounts[0].Source, cfg.Sandbox.BindMounts[0].Target)
	}
	if cfg.Sandbox.BindMounts[1].Source != "/easeshare/SH/Method1" || cfg.Sandbox.BindMounts[1].Target != "/srv/share/Method1" {
		t.Errorf("bind_mounts[1] = {%q, %q}", cfg.Sandbox.BindMounts[1].Source, cfg.Sandbox.BindMounts[1].Target)
	}
	if cfg.Sandbox.BindMounts[2].Source != "/easeshare/SIMU3/Sys_Run" || cfg.Sandbox.BindMounts[2].Target != "/easeshare/SIMU3/Sys_Run" {
		t.Errorf("bind_mounts[2] = {%q, %q}", cfg.Sandbox.BindMounts[2].Source, cfg.Sandbox.BindMounts[2].Target)
	}
}

func TestLoadConfigSandboxBindMountsInvalid(t *testing.T) {
	// 空字符串路径与不支持的类型（如数字）应解析失败。
	for _, tc := range []struct {
		name    string
		bindYML string
	}{
		{name: "empty string", bindYML: `- ""`},
		{name: "numeric scalar", bindYML: `- 123`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  bind_mounts:
`+tc.bindYML+`
`)
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("expected error for invalid bind mount %q, got nil", tc.bindYML)
			}
		})
	}
}

func TestLoadConfigSandboxUsernsInitPath(t *testing.T) {
	// sandbox 启用时解析 userns_init_path 配置；留空则保持空串（由运行时推导）。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  userns_init_path: "/custom/path/userns-init"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Error("sandbox.enabled should be true")
	}
	if cfg.Sandbox.UsernsInitPath != "/custom/path/userns-init" {
		t.Errorf("sandbox.userns_init_path = %q, want %q", cfg.Sandbox.UsernsInitPath, "/custom/path/userns-init")
	}
}

func TestLoadConfigSandboxUsernsInitPathEmpty(t *testing.T) {
	// 未显式设置 userns_init_path 时保持空串（运行时默认推导为
	// sandbox-init 同目录下的 userns-init），不设默认路径。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sandbox.UsernsInitPath != "" {
		t.Errorf("sandbox.userns_init_path = %q, want empty (runtime derivation)", cfg.Sandbox.UsernsInitPath)
	}
}

func TestLoadConfigSandboxProxyConfig(t *testing.T) {
	// 显式配置 http_proxy / https_proxy / no_proxy 时解析正确。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  http_proxy: "http://proxy.example.com:8080"
  https_proxy: "http://proxy.example.com:8080"
  no_proxy: "localhost,127.0.0.1"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sandbox.HTTPProxy != "http://proxy.example.com:8080" {
		t.Errorf("sandbox.http_proxy = %q, want %q", cfg.Sandbox.HTTPProxy, "http://proxy.example.com:8080")
	}
	if cfg.Sandbox.HTTPSProxy != "http://proxy.example.com:8080" {
		t.Errorf("sandbox.https_proxy = %q, want %q", cfg.Sandbox.HTTPSProxy, "http://proxy.example.com:8080")
	}
	if cfg.Sandbox.NoProxy != "localhost,127.0.0.1" {
		t.Errorf("sandbox.no_proxy = %q, want %q", cfg.Sandbox.NoProxy, "localhost,127.0.0.1")
	}
}

func TestLoadConfigSandboxProxyDefaults(t *testing.T) {
	// 未显式配置 http_proxy / https_proxy / no_proxy 时保持空字符串。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sandbox.HTTPProxy != "" {
		t.Errorf("sandbox.http_proxy = %q, want empty", cfg.Sandbox.HTTPProxy)
	}
	if cfg.Sandbox.HTTPSProxy != "" {
		t.Errorf("sandbox.https_proxy = %q, want empty", cfg.Sandbox.HTTPSProxy)
	}
	if cfg.Sandbox.NoProxy != "" {
		t.Errorf("sandbox.no_proxy = %q, want empty", cfg.Sandbox.NoProxy)
	}
}

func TestLoadConfigSandboxBindDefaults(t *testing.T) {
	// 未显式设置时 bind_hosts=false、bind_resolv=false、bind_mounts 为空。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sandbox.BindHosts {
		t.Error("sandbox.bind_hosts should default to false")
	}
	if cfg.Sandbox.BindResolv {
		t.Error("sandbox.bind_resolv should default to false")
	}
	if len(cfg.Sandbox.BindMounts) != 0 {
		t.Errorf("sandbox.bind_mounts len = %d, want 0", len(cfg.Sandbox.BindMounts))
	}
}

func TestLoadConfigAuditDefaults(t *testing.T) {
	// 未显式设置 audit：enabled 缺省 true，其余各默认值。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AuditEnabled {
		t.Error("audit.enabled should default to true")
	}
	if cfg.AuditDir != DefaultAuditDir {
		t.Errorf("AuditDir = %q, want %q", cfg.AuditDir, DefaultAuditDir)
	}
	if !cfg.AuditRecordIn {
		t.Error("AuditRecordIn should default to true")
	}
	if cfg.AuditPasswordMode != DefaultAuditPasswordInput {
		t.Errorf("AuditPasswordMode = %q, want %q", cfg.AuditPasswordMode, DefaultAuditPasswordInput)
	}
	if cfg.AuditPasswordMaskWhen != DefaultAuditPasswordMaskWhen {
		t.Errorf("AuditPasswordMaskWhen = %q, want %q", cfg.AuditPasswordMaskWhen, DefaultAuditPasswordMaskWhen)
	}
	if cfg.AuditMaxBytes != DefaultAuditMaxSessionBytes {
		t.Errorf("AuditMaxBytes = %d, want %d", cfg.AuditMaxBytes, DefaultAuditMaxSessionBytes)
	}
	if cfg.AuditTotalBytes != DefaultAuditMaxTotalBytes {
		t.Errorf("AuditTotalBytes = %d, want %d", cfg.AuditTotalBytes, DefaultAuditMaxTotalBytes)
	}
	if cfg.AuditRetention != DefaultAuditRetention {
		t.Errorf("AuditRetention = %v, want %v", cfg.AuditRetention, DefaultAuditRetention)
	}
	if cfg.AuditCleanupIvl != DefaultAuditCleanupInterval {
		t.Errorf("AuditCleanupIvl = %v, want %v", cfg.AuditCleanupIvl, DefaultAuditCleanupInterval)
	}
	if cfg.AuditFailClosed {
		t.Error("AuditFailClosed should default to false")
	}
	if cfg.AuditTrustProxy {
		t.Error("AuditTrustProxy should default to false")
	}
	if !cfg.AuditUserSelf {
		t.Error("AuditUserSelf should default to true")
	}
	if len(cfg.AuditAdmins) != 0 {
		t.Errorf("AuditAdmins len = %d, want 0", len(cfg.AuditAdmins))
	}
}

func TestLoadConfigAuditEVWDefaults(t *testing.T) {
	// 未显式设置：password_mask_when 默认 echo_verify，hang 兜底 2000ms、策略 mask，
	// 严格整块关闭，TUI 门关闭。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AuditPasswordMaskWhen != "echo_verify" {
		t.Errorf("AuditPasswordMaskWhen = %q, want echo_verify", cfg.AuditPasswordMaskWhen)
	}
	if cfg.AuditEchoVerifyTimeout != 2000*time.Millisecond {
		t.Errorf("AuditEchoVerifyTimeout = %v, want 2000ms", cfg.AuditEchoVerifyTimeout)
	}
	if cfg.AuditEchoVerifyHangPolicy != "mask" {
		t.Errorf("AuditEchoVerifyHangPolicy = %q, want mask", cfg.AuditEchoVerifyHangPolicy)
	}
	if cfg.AuditEchoVerifyStrictBatch {
		t.Error("AuditEchoVerifyStrictBatch should default to false")
	}
	if cfg.AuditEchoVerifyTUI {
		t.Error("AuditEchoVerifyTUI should default to false")
	}
}

func TestLoadConfigAuditEVWExplicit(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
audit:
  enabled: true
  password_mask_when: echo_verify_tui
  echo_verify_hang_ms: 250
  echo_verify_hang_policy: reached_plain
  echo_verify_strict_batch: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AuditPasswordMaskWhen != "echo_verify_tui" {
		t.Errorf("AuditPasswordMaskWhen = %q, want echo_verify_tui", cfg.AuditPasswordMaskWhen)
	}
	if cfg.AuditEchoVerifyTimeout != 250*time.Millisecond {
		t.Errorf("AuditEchoVerifyTimeout = %v, want 250ms", cfg.AuditEchoVerifyTimeout)
	}
	if cfg.AuditEchoVerifyHangPolicy != "reached_plain" {
		t.Errorf("AuditEchoVerifyHangPolicy = %q, want reached_plain", cfg.AuditEchoVerifyHangPolicy)
	}
	if !cfg.AuditEchoVerifyStrictBatch {
		t.Error("AuditEchoVerifyStrictBatch should be true")
	}
	// echo_verify_tui 判据隐式开启 TUI 门。
	if !cfg.AuditEchoVerifyTUI {
		t.Error("echo_verify_tui should imply AuditEchoVerifyTUI=true")
	}
}

func TestLoadConfigAuditEVWLegacyKey(t *testing.T) {
	// 旧键 echo_verify_latency_ms 作为兼容别名，映射到 hang 兜底语义。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
audit:
  enabled: true
  echo_verify_latency_ms: 300
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AuditEchoVerifyTimeout != 300*time.Millisecond {
		t.Errorf("legacy echo_verify_latency_ms: AuditEchoVerifyTimeout = %v, want 300ms", cfg.AuditEchoVerifyTimeout)
	}
	// 旧键未设置时不应影响默认策略。
	if cfg.AuditEchoVerifyHangPolicy != "mask" {
		t.Errorf("AuditEchoVerifyHangPolicy = %q, want mask", cfg.AuditEchoVerifyHangPolicy)
	}
}

func TestLoadConfigAuditEVWEnumValid(t *testing.T) {
	// 四个合法判据均应通过校验。
	for _, v := range []string{"echo_verify", "echo_verify_tui", "echo_off", "echo_off_canonical"} {
		t.Run(v, func(t *testing.T) {
			path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
audit:
  enabled: true
  password_mask_when: `+v+"\n")
			if _, err := LoadConfig(path); err != nil {
				t.Fatalf("enum %q should be valid, got %v", v, err)
			}
		})
	}
}

func TestLoadConfigAuditExplicit(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
audit:
  enabled: false
  dir: "/tmp/audit"
  record_input: false
  password_input: omit
  password_mask_when: echo_off
  max_session_bytes: "5M"
  max_total_bytes: "2G"
  retention_days: 0
  cleanup_interval: "30m"
  fail_closed: true
  trust_proxy: true
  user_can_view_self: false
  admins: ["Alice", "AUDITOR"]
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AuditEnabled {
		t.Error("audit.enabled should be false")
	}
	if cfg.AuditDir != "/tmp/audit" {
		t.Errorf("AuditDir = %q", cfg.AuditDir)
	}
	if cfg.AuditRecordIn {
		t.Error("AuditRecordIn should be false")
	}
	if cfg.AuditPasswordMode != "omit" {
		t.Errorf("AuditPasswordMode = %q", cfg.AuditPasswordMode)
	}
	if cfg.AuditPasswordMaskWhen != "echo_off" {
		t.Errorf("AuditPasswordMaskWhen = %q", cfg.AuditPasswordMaskWhen)
	}
	if cfg.AuditMaxBytes != 5<<20 {
		t.Errorf("AuditMaxBytes = %d, want %d", cfg.AuditMaxBytes, 5<<20)
	}
	if cfg.AuditTotalBytes != 2<<30 {
		t.Errorf("AuditTotalBytes = %d, want %d", cfg.AuditTotalBytes, 2<<30)
	}
	if cfg.AuditRetention != 0 {
		t.Errorf("AuditRetention = %v, want 0 (no cleanup)", cfg.AuditRetention)
	}
	if cfg.AuditCleanupIvl != 30*time.Minute {
		t.Errorf("AuditCleanupIvl = %v, want 30m", cfg.AuditCleanupIvl)
	}
	if !cfg.AuditFailClosed || !cfg.AuditTrustProxy {
		t.Error("AuditFailClosed / AuditTrustProxy should be true")
	}
	if cfg.AuditUserSelf {
		t.Error("AuditUserSelf should be false")
	}
	// 白名单小写归一，IsAuditor 大小写不敏感。
	if !cfg.AuditAdmins["alice"] || !cfg.AuditAdmins["auditor"] {
		t.Errorf("AuditAdmins = %v, want lowercase keys", cfg.AuditAdmins)
	}
	if !cfg.IsAuditor("ALICE") || !cfg.IsAuditor("auditor") {
		t.Error("IsAuditor should be case-insensitive")
	}
	if cfg.IsAuditor("bob") {
		t.Error("IsAuditor(bob) should be false")
	}
}

func TestLoadConfigAuditEnabledExplicitFalse(t *testing.T) {
	// enabled: false 时不校验枚举（无埋点，零开销）。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
audit:
  enabled: false
  password_input: bogus
`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("disabled audit should skip enum validation, got %v", err)
	}
}

func TestLoadConfigAuditInvalidEnum(t *testing.T) {
	for _, tc := range []struct {
		name string
		yml  string
	}{
		{name: "password_input", yml: "  password_input: bogus\n"},
		{name: "password_mask_when", yml: "  password_mask_when: bogus\n"},
		{name: "echo_verify_hang_policy", yml: "  echo_verify_hang_policy: bogus\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
audit:
  enabled: true
`+tc.yml)
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("expected error for invalid %s, got nil", tc.name)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		def  int64
		want int64
		ok   bool
	}{
		{in: "", def: 42, want: 42, ok: true},
		{in: "100M", want: 100 << 20, ok: true},
		{in: "20G", want: 20 << 30, ok: true},
		{in: "1T", want: 1 << 40, ok: true},
		{in: "512K", want: 512 << 10, ok: true},
		{in: "100MB", want: 100 << 20, ok: true},
		{in: "1024", want: 1024, ok: true},
		{in: "2g", want: 2 << 30, ok: true},
		{in: "1_000M", want: 1000 << 20, ok: true},
		{in: "abc", ok: false},
		{in: "10X", ok: false},
		{in: "M", ok: false},
	} {
		got, err := parseSize(tc.in, tc.def)
		if tc.ok && err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("parseSize(%q) expected error, got %d", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestExampleConfigParse(t *testing.T) {
	// ExampleConfig 必须可直接解析，且 audit 块字段齐全（防示例漂移）。
	path := writeTempConfig(t, ExampleConfig)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(ExampleConfig): %v", err)
	}
	if !cfg.AuditEnabled {
		t.Error("ExampleConfig audit.enabled should parse to true")
	}
	if cfg.AuditDir != DefaultAuditDir {
		t.Errorf("ExampleConfig AuditDir = %q, want %q", cfg.AuditDir, DefaultAuditDir)
	}
	if !cfg.IsAuditor("alice") || !cfg.IsAuditor("auditor") {
		t.Errorf("ExampleConfig admins not parsed: %v", cfg.AuditAdmins)
	}
	// 登录白名单字段（防示例漂移）。
	if cfg.WebUsersFile != "/etc/webshell/web_users" {
		t.Errorf("ExampleConfig WebUsersFile = %q, want /etc/webshell/web_users", cfg.WebUsersFile)
	}
	if cfg.WebUsersFailOpen {
		t.Error("ExampleConfig web_users_fail_open should default to false")
	}
}

func TestLoadConfigWebUsersDefaults(t *testing.T) {
	// 未显式设置：web_users_file 为空（未配置），web_users_fail_open 缺省 false。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.WebUsersFile != "" {
		t.Errorf("WebUsersFile = %q, want empty (unconfigured)", cfg.WebUsersFile)
	}
	if cfg.WebUsersFailOpen {
		t.Error("WebUsersFailOpen should default to false")
	}
}

func TestLoadConfigWebUsersExplicit(t *testing.T) {
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
web_users_file: "/etc/webshell/web_users"
web_users_fail_open: true
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.WebUsersFile != "/etc/webshell/web_users" {
		t.Errorf("WebUsersFile = %q, want /etc/webshell/web_users", cfg.WebUsersFile)
	}
	if !cfg.WebUsersFailOpen {
		t.Error("WebUsersFailOpen should be true")
	}
}

func TestLoadConfigUsersGroupKeyIgnored(t *testing.T) {
	// 旧键 users_group 已移除：文件即使含该键也被忽略，不生效、不报错。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
users_group: "webusers"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with legacy users_group: %v", err)
	}
	if cfg.WebUsersFile != "" {
		t.Errorf("legacy users_group must not affect WebUsersFile; got %q", cfg.WebUsersFile)
	}
	if cfg.WebUsersFailOpen {
		t.Error("legacy users_group must not affect WebUsersFailOpen")
	}
}
