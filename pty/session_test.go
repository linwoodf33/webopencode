package pty

import (
	"strings"
	"testing"

	"webshell/auth"
)

func TestSyncCodexCommand(t *testing.T) {
	const (
		home     = "/home/testuser"
		syncFrom = "/share/apps/.codex-config"
	)
	dest := home + "/.codex"

	cmd := syncCodexCommand("testuser", home, syncFrom, true)

	// 落盘目标为 ~/.codex（注意不是 ~/.config/codex）。
	if !strings.Contains(cmd, "mkdir -p "+dest) {
		t.Errorf("syncCodexCommand missing mkdir -p %s: %s", dest, cmd)
	}
	// 以 config.toml 存在性标记做 if 保护（幂等，已初始化则跳过）。
	if !strings.Contains(cmd, "if [ ! -f "+dest+"/config.toml ]") {
		t.Errorf("syncCodexCommand missing config.toml marker guard: %s", cmd)
	}
	// 白名单：只复制 config.toml 与 skills/（与 opencode 只同步 jsonc+skills 对齐）。
	if !strings.Contains(cmd, "cp "+syncFrom+"/config.toml "+dest+"/config.toml") {
		t.Errorf("syncCodexCommand missing config.toml copy: %s", cmd)
	}
	if !strings.Contains(cmd, "cp -r "+syncFrom+"/skills "+dest+"/skills") {
		t.Errorf("syncCodexCommand missing skills copy: %s", cmd)
	}
	// 白名单同步：绝不含 tar 排除清单，绝不含任何运行态/敏感文件名
	// （sqlite/jsonl/db/logs/sessions/history/cache/rollout 等）。
	for _, forbidden := range []string{
		"tar",
		"--exclude",
		"sqlite",
		".jsonl",
		".db",
		"logs",
		"sessions",
		"history",
		"installation_id",
		"version.json",
		"shell_snapshots",
		"thread-writer-locks",
		"auth.json",
		"cache",
		"rollout",
	} {
		if strings.Contains(cmd, forbidden) {
			t.Errorf("syncCodexCommand should not contain %q (whitelist only): %s", forbidden, cmd)
		}
	}
	// 命令以 `fi` 收尾（if 结构闭合）。
	if !strings.HasSuffix(cmd, "fi") {
		t.Errorf("syncCodexCommand should end with 'fi': %s", cmd)
	}
}

func TestSyncCodexCommandOverwriteEquivalent(t *testing.T) {
	// overwrite=false 命令形态与 overwrite=true 行为等价（codex 模式下
	// 以 config.toml 存在性标记为主，overwrite 影响甚微，文档化）。
	a := syncCodexCommand("testuser", "/home/testuser", "/share/apps/.codex-config", true)
	b := syncCodexCommand("testuser", "/home/testuser", "/share/apps/.codex-config", false)
	if a != b {
		t.Errorf("syncCodexCommand overwrite=true/false differ:\n true:  %s\n false: %s", a, b)
	}
}

func TestSyncCodexConfigEmptyArgs(t *testing.T) {
	c := &auth.CodexConfig{Enabled: true}
	if err := SyncCodexConfig("", "/home/testuser", c, 0); err == nil {
		t.Error("SyncCodexConfig with empty username should return error")
	}
	if err := SyncCodexConfig("testuser", "", c, 0); err == nil {
		t.Error("SyncCodexConfig with empty homeDir should return error")
	}
	if err := SyncCodexConfig("testuser", "/home/testuser", c, 0); err == nil {
		t.Error("SyncCodexConfig with empty sync_from should return error")
	}
	// codex 为 nil / 未启用：直接返回 nil，不执行任何同步。
	if err := SyncCodexConfig("testuser", "/home/testuser", nil, 0); err != nil {
		t.Errorf("SyncCodexConfig(nil) = %v, want nil", err)
	}
	disabled := &auth.CodexConfig{Enabled: false}
	if err := SyncCodexConfig("testuser", "/home/testuser", disabled, 0); err != nil {
		t.Errorf("SyncCodexConfig(disabled) = %v, want nil", err)
	}
}
