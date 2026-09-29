package pty

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/creack/pty"

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

// TestForceResizeCurrent 用注入的 get/set 接缝覆盖 ForceResizeCurrent 的各分支。
//
// 关键不变量（修复规格）：序列末尾两次 setsize 必须是**非零→非零且尺寸不同**
// 的变更，才能在本沙箱（userns+pidns）下真正投递 SIGWINCH。0×0 场景必须被
// 归一为非零基准后再做两次非零→非零抖动，而非只设一次默认尺寸。
//
// 注意注入的 ptyGetsize 返回顺序为 (rows, cols, err)，与 creack/pty 一致。
func TestForceResizeCurrent(t *testing.T) {
	// 保存并在用例结束后恢复全局接缝，避免跨测试污染。
	origGet, origSet := ptyGetsize, ptySetsize
	t.Cleanup(func() { ptyGetsize, ptySetsize = origGet, origSet })

	// master 仅需非 nil（get/set 已被替换，不会真正触达 fd）。
	newSess := func() *Session { return &Session{master: &os.File{}} }

	// runCase 注入 get=(rows,cols)，返回捕获到的 setsize 序列。
	runCase := func(t *testing.T, rows, cols int, getErr error) []pty.Winsize {
		t.Helper()
		var sizes []pty.Winsize
		ptyGetsize = func(*os.File) (int, int, error) { return rows, cols, getErr }
		ptySetsize = func(_ *os.File, ws *pty.Winsize) error { sizes = append(sizes, *ws); return nil }
		if err := newSess().ForceResizeCurrent(); err != nil {
			t.Fatalf("ForceResizeCurrent: %v", err)
		}
		return sizes
	}

	// assertSignalingTail 校验 setsize 序列形状：最终尺寸为基准 (cols,rows)，
	// 且最后两次为不同的非零→非零变更（保证 SIGWINCH 可投递）。
	assertSignalingTail := func(t *testing.T, sizes []pty.Winsize, cols, rows int) {
		t.Helper()
		if len(sizes) != 3 {
			t.Fatalf("set calls = %d, want 3 (baseline + jitter + restore): %+v", len(sizes), sizes)
		}
		base, jitter, last := sizes[0], sizes[1], sizes[2]
		if base.Cols != uint16(cols) || base.Rows != uint16(rows) {
			t.Errorf("baseline set = %+v, want cols=%d rows=%d", base, cols, rows)
		}
		// 最后两次必须不同（真实变化 → 内核 tty_do_resize 不短路 → SIGWINCH）。
		if jitter == last {
			t.Errorf("last two sets must differ to emit SIGWINCH, got %+v twice", jitter)
		}
		if last != base {
			t.Errorf("final set = %+v, want restore to baseline %+v", last, base)
		}
		// 最后两次均须为非零（本沙箱下非零→非零才投递信号）。
		for i, ws := range []pty.Winsize{jitter, last} {
			if ws.Cols < 1 || ws.Rows < 1 {
				t.Errorf("signaling set[%d] = %+v, must be non-zero", i, ws)
			}
		}
	}

	t.Run("existing-size", func(t *testing.T) {
		// 常规非零 (cols=54, rows=121)：54,121 → 53,120 → 54,121。
		sizes := runCase(t, 121, 54, nil)
		assertSignalingTail(t, sizes, 54, 121)
		if sizes[1].Cols != 53 || sizes[1].Rows != 120 {
			t.Errorf("jitter set = %+v, want cols=53 rows=120", sizes[1])
		}
	})

	t.Run("never-resized", func(t *testing.T) {
		// 关键：0×0 由 get 返回 → 归一为 80x24，再做非零→非零抖动：
		// 80,24 → 79,23 → 80,24（修复前只 Setsize(80,24) 一次，无 SIGWINCH）。
		sizes := runCase(t, 0, 0, nil)
		assertSignalingTail(t, sizes, 80, 24)
		if sizes[1].Cols != 79 || sizes[1].Rows != 23 {
			t.Errorf("jitter set = %+v, want cols=79 rows=23", sizes[1])
		}
	})

	t.Run("one-by-one-boundary", func(t *testing.T) {
		// (cols=1, rows=1)：无维度可减，派生必须 != (1,1) 且两维 >=1（不得出现 0）。
		sizes := runCase(t, 1, 1, nil)
		assertSignalingTail(t, sizes, 1, 1)
		jitter := sizes[1]
		if jitter == sizes[0] {
			t.Errorf("jitter = %+v must differ from (1,1)", jitter)
		}
		for i, ws := range sizes {
			if ws.Cols < 1 || ws.Rows < 1 {
				t.Errorf("set[%d] = %+v, want cols/rows >= 1", i, ws)
			}
		}
	})

	t.Run("one-column-boundary", func(t *testing.T) {
		// (cols=1, rows=5)：cols 保持 1，仅 rows 变化 → 抖动应 != (1,5) 且 >=1。
		sizes := runCase(t, 5, 1, nil)
		assertSignalingTail(t, sizes, 1, 5)
		jitter := sizes[1]
		if jitter == sizes[0] {
			t.Errorf("jitter = %+v must differ from (1,5)", jitter)
		}
		for i, ws := range sizes {
			if ws.Cols < 1 || ws.Rows < 1 {
				t.Errorf("set[%d] = %+v, want cols/rows >= 1", i, ws)
			}
		}
	})

	t.Run("closed", func(t *testing.T) {
		called := false
		ptyGetsize = func(*os.File) (int, int, error) { called = true; return 0, 0, nil }
		ptySetsize = func(*os.File, *pty.Winsize) error { called = true; return nil }

		s := newSess()
		s.closed = true
		if err := s.ForceResizeCurrent(); err == nil {
			t.Error("ForceResizeCurrent on closed session should return error")
		}
		if called {
			t.Error("closed session should not touch pty get/set")
		}
	})

	t.Run("get-error", func(t *testing.T) {
		// get 失败：直接返回 error，不调用 set。
		sentinel := os.ErrInvalid
		setCalled := false
		ptyGetsize = func(*os.File) (int, int, error) { return 0, 0, sentinel }
		ptySetsize = func(*os.File, *pty.Winsize) error { setCalled = true; return nil }
		if err := newSess().ForceResizeCurrent(); err != sentinel {
			t.Errorf("ForceResizeCurrent get-error = %v, want %v", err, sentinel)
		}
		if setCalled {
			t.Error("setsize must not be called when getsize fails")
		}
	})
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
