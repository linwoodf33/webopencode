package main

import "testing"

func TestValidateCodexProgram(t *testing.T) {
	base := &Config{
		UID:        1000,
		GID:        1000,
		Home:       "/home/testuser",
		Session:    "test-session-1",
		Network:    "none",
		Rootfs:     "/opt/runner/rootfs/base",
		CgroupRoot: "/sys/fs/cgroup/webshell_sandbox",
	}

	// codex 程序但 path 为空 -> error。
	cfg := *base
	cfg.Program = "codex"
	cfg.CodexPath = ""
	if err := validate(&cfg); err == nil {
		t.Error("validate should error for codex program with empty path")
	}

	// codex 程序且 path 非空 -> 通过。
	cfg = *base
	cfg.Program = "codex"
	cfg.CodexPath = "/share/apps/.codex-standalone/codex"
	if err := validate(&cfg); err != nil {
		t.Errorf("validate should pass for codex program with path, got: %v", err)
	}

	// opencode 分支保持原样（回归）。
	cfg = *base
	cfg.Program = "opencode"
	cfg.OpencodePath = "/opt/opencode/bin/opencode"
	if err := validate(&cfg); err != nil {
		t.Errorf("validate should pass for opencode program with path, got: %v", err)
	}

	// bash 分支保持原样（回归）。
	cfg = *base
	cfg.Program = "bash"
	if err := validate(&cfg); err != nil {
		t.Errorf("validate should pass for bash program, got: %v", err)
	}
}
