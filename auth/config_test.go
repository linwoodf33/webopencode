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
token_ttl: 120
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
	if cfg.TokenTTLd != 120*time.Second {
		t.Errorf("TokenTTLd = %v, want 120s", cfg.TokenTTLd)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// 未显式设置 timeout / token_ttl / listen 时走默认值。
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
	if cfg.TokenTTLd != DefaultTokenTTL {
		t.Errorf("TokenTTLd = %v, want default %v", cfg.TokenTTLd, DefaultTokenTTL)
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

func TestLoadConfigFileNotFound(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestLoadConfigSandboxBindMounts(t *testing.T) {
	// sandbox 启用时解析 bind_hosts / bind_mounts 配置。
	path := writeTempConfig(t, `
ad:
  server: "ldaps.example.com:636"
  manager_dn: "dn"
  manager_password: "pw"
  search_dn: "dc=base"
sandbox:
  enabled: true
  bind_hosts: true
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

func TestLoadConfigSandboxBindDefaults(t *testing.T) {
	// 未显式设置时 bind_hosts=false、bind_mounts 为空。
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
	if len(cfg.Sandbox.BindMounts) != 0 {
		t.Errorf("sandbox.bind_mounts len = %d, want 0", len(cfg.Sandbox.BindMounts))
	}
}
