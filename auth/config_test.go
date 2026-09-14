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
