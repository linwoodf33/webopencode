package pty

import (
	"os/exec"
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

	// 关键修复：命令必须完全单行（换行会在 sudo -i bash -c 链中丢失，
	// 导致 fi\nif 粘连成 fiif 的 bash 语法错误）。
	if strings.Contains(cmd, "\n") {
		t.Errorf("syncCodexCommand must be a single line (newline lost in sudo -i bash -c), got: %q", cmd)
	}

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
	// AGENTS.md 补全片段：独立于整体跳过标记，每次启动都检查；目标缺失且源存在才复制。
	// 断言片段形态（缺失才补 + 源存在条件）。
	if !strings.Contains(cmd, "if [ ! -f "+dest+"/AGENTS.md ] && [ -f "+syncFrom+"/AGENTS.md ]") {
		t.Errorf("syncCodexCommand missing AGENTS.md guard snippet: %s", cmd)
	}
	// AGENTS.md 片段以分号分隔拼接在整体 fi 之后（单行，无换行）。
	if !strings.Contains(cmd, "; fi; if [ ! -f "+dest+"/AGENTS.md ]") {
		t.Errorf("syncCodexCommand missing '; fi; if [ ! -f %s/AGENTS.md ]' separator: %s", dest, cmd)
	}
	// 片段含防御性 mkdir -p <dest> 与 cp <syncFrom>/AGENTS.md <dest>/AGENTS.md。
	if !strings.Contains(cmd, "mkdir -p "+dest+" && cp "+syncFrom+"/AGENTS.md "+dest+"/AGENTS.md") {
		t.Errorf("syncCodexCommand missing AGENTS.md mkdir/cp: %s", cmd)
	}
	// agents 目录补全片段（策略 A，agents 复数）：源 <syncFrom>/agents 存在且非空、
	// 目标 <dest>/agents 不存在才整体复制；目标已有则跳过（保留用户自定义 agent）；
	// 源目录为空时不创建目标空目录。
	if !strings.Contains(cmd, "if [ -d "+syncFrom+"/agents ] && [ ! -d "+dest+"/agents ]") {
		t.Errorf("syncCodexCommand missing agents dir guard snippet: %s", cmd)
	}
	// agents 片段同样以分号分隔拼接在 AGENTS.md 片段之后（单行，无换行）。
	if !strings.Contains(cmd, "; if [ -d "+syncFrom+"/agents ]") {
		t.Errorf("syncCodexCommand missing '; if [ -d %s/agents ]' separator: %s", syncFrom, cmd)
	}
	if !strings.Contains(cmd, "cp -r "+syncFrom+"/agents "+dest+"/agents") {
		t.Errorf("syncCodexCommand missing agents dir copy: %s", cmd)
	}
	// agents 片段含空目录判定 ls -A <syncFrom>/agents。
	if !strings.Contains(cmd, "ls -A "+syncFrom+"/agents") {
		t.Errorf("syncCodexCommand missing agents dir non-empty check: %s", cmd)
	}
	// AGENTS.md 与 agents 片段均追加在整体同步（config.toml 幂等标记）闭合 fi 之后：
	// 即整体 if...fi 先于两片段执行；即使 config.toml 已存在导致整体跳过，两片段仍会执行。
	// 单行形态下以分号 ; 分隔（"…; fi; if …"）。
	sep := "; fi; if [ ! -f " + dest + "/AGENTS.md ]" // 整体闭合 fi + AGENTS.md 片段起点
	overallClose := strings.Index(cmd, sep)
	if overallClose < 0 {
		t.Fatalf("syncCodexCommand missing '; fi;' separator before AGENTS.md snippet: %s", cmd)
	}
	if strings.Index(cmd, agentsSyncSnippet(dest, syncFrom)) < overallClose {
		t.Errorf("syncCodexCommand AGENTS.md snippet should follow the overall 'fi': %s", cmd)
	}
	if strings.Index(cmd, agentDirSyncSnippet(dest, syncFrom, "agents")) < overallClose {
		t.Errorf("syncCodexCommand agents snippet should follow the overall 'fi': %s", cmd)
	}
	// 命令以合法 shell 结构结尾：agents 目录片段自带 if/fi 闭合（非整体同步的裸 fi）。
	if !strings.HasSuffix(cmd, agentDirSyncSnippet(dest, syncFrom, "agents")) {
		t.Errorf("syncCodexCommand should end with the agents dir snippet: %s", cmd)
	}
	// 命令以 `fi` 收尾（if 结构闭合）。
	if !strings.HasSuffix(cmd, "fi") {
		t.Errorf("syncCodexCommand should end with 'fi': %s", cmd)
	}
}

func TestSyncOpencodeCommand(t *testing.T) {
	const (
		home     = "/home/testuser"
		syncFrom = "/share/apps/.opencode-config"
	)
	dest := home + "/.config/opencode"

	for _, overwrite := range []bool{true, false} {
		cmd := syncConfigCommand("testuser", home, syncFrom, overwrite)

		// 关键修复：命令必须完全单行（换行会在 sudo -i bash -c 链中丢失，
		// 导致 fi\nif 粘连成 fiif 的 bash 语法错误）。
		if strings.Contains(cmd, "\n") {
			t.Errorf("syncConfigCommand(overwrite=%v) must be a single line (newline lost in sudo -i bash -c), got: %q", overwrite, cmd)
		}
		// AGENTS.md 片段以分号分隔拼接在整体 fi 之后（单行，无换行）。
		if !strings.Contains(cmd, "; fi; if [ ! -f "+dest+"/AGENTS.md ]") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing '; fi; if [ ! -f %s/AGENTS.md ]' separator: %s", overwrite, dest, cmd)
		}

		// 整体跳过标记保持（回归）：skills 目录存在即跳过全部同步（已初始化）。
		if !strings.Contains(cmd, "if [ ! -d "+dest+"/skills ]") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing skills marker guard: %s", overwrite, cmd)
		}
		// AGENTS.md 补全片段：独立于整体跳过标记，每次启动都检查；缺失才补 + 源存在条件。
		if !strings.Contains(cmd, "if [ ! -f "+dest+"/AGENTS.md ] && [ -f "+syncFrom+"/AGENTS.md ]") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing AGENTS.md guard snippet: %s", overwrite, cmd)
		}
		// 片段含防御性 mkdir -p <dest> 与 cp <syncFrom>/AGENTS.md <dest>/AGENTS.md。
		if !strings.Contains(cmd, "mkdir -p "+dest+" && cp "+syncFrom+"/AGENTS.md "+dest+"/AGENTS.md") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing AGENTS.md mkdir/cp: %s", overwrite, cmd)
		}
		// agent 目录补全片段（策略 A，agent 单数）：源 <syncFrom>/agent 存在且非空、
		// 目标 <dest>/agent 不存在才整体复制；目标已有则跳过（保留用户自定义 agent）；
		// 源目录为空时不创建目标空目录。
		if !strings.Contains(cmd, "if [ -d "+syncFrom+"/agent ] && [ ! -d "+dest+"/agent ]") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing agent dir guard snippet: %s", overwrite, cmd)
		}
		// agent 片段同样以分号分隔拼接在 AGENTS.md 片段之后（单行，无换行）。
		if !strings.Contains(cmd, "; if [ -d "+syncFrom+"/agent ]") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing '; if [ -d %s/agent ]' separator: %s", overwrite, syncFrom, cmd)
		}
		if !strings.Contains(cmd, "cp -r "+syncFrom+"/agent "+dest+"/agent") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing agent dir copy: %s", overwrite, cmd)
		}
		// agent 片段含空目录判定 ls -A <syncFrom>/agent。
		if !strings.Contains(cmd, "ls -A "+syncFrom+"/agent") {
			t.Errorf("syncConfigCommand(overwrite=%v) missing agent dir non-empty check: %s", overwrite, cmd)
		}
		// 两片段均追加在整体 if...fi 之后（作为命令后缀，自带 if/fi 闭合，合法 shell 结构）。
		if !strings.HasSuffix(cmd, agentDirSyncSnippet(dest, syncFrom, "agent")) {
			t.Errorf("syncConfigCommand(overwrite=%v) should end with the agent dir snippet: %s", overwrite, cmd)
		}
	}

	// overwrite=true/false 两分支命令均含 AGENTS.md 与 agent 目录片段，且片段一致（与 overwrite 无关）。
	a := syncConfigCommand("testuser", home, syncFrom, true)
	b := syncConfigCommand("testuser", home, syncFrom, false)
	if !strings.HasSuffix(a, agentDirSyncSnippet(dest, syncFrom, "agent")) || !strings.HasSuffix(b, agentDirSyncSnippet(dest, syncFrom, "agent")) {
		t.Error("syncConfigCommand both overwrite branches should end with the same agent dir snippet")
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

// TestSyncCommandsSingleLineShellSyntax 校验所有同步命令为**完全单行**且 shell 语法合法。
//
// 背景：同步命令经 `sudo -n -u <user> -i bash -c '<cmd>'` 执行时，命令中的换行 \n
// 会被吞掉（fi\nif 粘连成 fiif），导致 bash 语法错误、同步失败（ws.go 仅 log 不阻塞，
// 表现为"没同步"）。因此命令必须完全单行、以分号 ; 分隔，本测试防止该问题回归。
//
// bash -n 只做语法检查、不执行任何命令，安全。环境无 bash 时跳过（生产为 linux 必有）。
func TestSyncCommandsSingleLineShellSyntax(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available, skipping syntax check")
	}

	const (
		home         = "/home/testuser"
		opencodeFrom = "/share/apps/.opencode-config"
		codexFrom    = "/share/apps/.codex-config"
	)
	cases := []struct {
		name string
		cmd  string
	}{
		{"opencode-overwrite-true", syncConfigCommand("testuser", home, opencodeFrom, true)},
		{"opencode-overwrite-false", syncConfigCommand("testuser", home, opencodeFrom, false)},
		{"codex", syncCodexCommand("testuser", home, codexFrom, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 必须完全单行：换行会在 sudo -i bash -c 链中丢失。
			if strings.Contains(tc.cmd, "\n") {
				t.Fatalf("sync command must be a single line (newline lost in sudo -i bash -c), got: %q", tc.cmd)
			}
			// bash -n 语法校验（不执行），防止 fi\nif 粘连类语法错误回归。
			if err := exec.Command(bashPath, "-n", "-c", tc.cmd).Run(); err != nil {
				t.Errorf("sync command fails bash -n syntax check: %v\ncmd: %s", err, tc.cmd)
			}
		})
	}
}
