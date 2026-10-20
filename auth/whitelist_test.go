package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeWLFile 写入白名单文件并返回路径。
func writeWLFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "web_users")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write whitelist: %v", err)
	}
	return path
}

// bumpMtime 将文件 mtime 前移 d，确保惰性变化检测能观察到（跨平台粒度兜底）。
func bumpMtime(t *testing.T, path string, d time.Duration) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat whitelist: %v", err)
	}
	nt := fi.ModTime().Add(d)
	if err := os.Chtimes(path, nt, nt); err != nil {
		t.Fatalf("chtimes whitelist: %v", err)
	}
}

func TestParseWhitelist(t *testing.T) {
	// 覆盖：注释行、多用户、首尾空白、CRLF、空行、大小写归一/去重，
	// 以及非法行（含空格、数字开头、行尾 #、超长）跳过。
	data := []byte("# comment\n" +
		"alice\n" +
		"  bob  \n" +
		"carol\r\n" +
		"\n" +
		"ALICE\n" +
		"dave_1\n" +
		"eve-x\n" +
		"_underscore\n" +
		"bad name\n" +
		"1nope\n" +
		"alice # tail\n" +
		"this-name-is-way-too-long-for-a-username-exceeding-32-chars\n" +
		"#dave_1\n")
	m := parseWhitelist(data)
	want := []string{"alice", "bob", "carol", "dave_1", "eve-x", "_underscore"}
	for _, u := range want {
		if _, ok := m[u]; !ok {
			t.Errorf("parseWhitelist missing %q; got %v", u, keys(m))
		}
	}
	if len(m) != len(want) {
		t.Errorf("parseWhitelist len = %d, want %d (%v)", len(m), len(want), keys(m))
	}
	// 非法行不应进入 map。
	if _, ok := m["1nope"]; ok {
		t.Error("invalid line '1nope' should be skipped")
	}
	if _, ok := m["bad name"]; ok {
		t.Error("invalid line 'bad name' should be skipped")
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWhitelistCaseInsensitive(t *testing.T) {
	// 清单含 Alice / Bob（混合大小写，解析时归一为小写）；查询任意大小写均应命中。
	path := writeWLFile(t, "Alice\nBob\n")
	w := newUserWhitelist(path, false)
	if !w.Allowed("alice") {
		t.Error("query alice against list Alice should be allowed")
	}
	if !w.Allowed("BOB") {
		t.Error("query BOB against list Bob should be allowed")
	}
	if !w.Allowed("Alice") {
		t.Error("query Alice against list Alice should be allowed")
	}
}

func TestWhitelistExactMatch(t *testing.T) {
	path := writeWLFile(t, "alice\n")
	w := newUserWhitelist(path, false)
	if !w.Allowed("alice") {
		t.Fatal("alice should be allowed")
	}
	for _, q := range []string{"alice1", "alice_", "alice-", "Aalice", "alic"} {
		if w.Allowed(q) {
			t.Errorf("query %q should be denied (exact match only)", q)
		}
	}
}

func TestWhitelistStates(t *testing.T) {
	// 未配置 + 非 fail-open → 全拒。
	if w := newUserWhitelist("", false); w.Allowed("alice") {
		t.Error("unconfigured whitelist should deny (fail-closed)")
	}
	// fail-open → 恒放行（即使未配置 / 文件不存在）。
	if w := newUserWhitelist("", true); !w.Allowed("alice") {
		t.Error("fail-open whitelist should allow")
	}
	if w := newUserWhitelist(filepath.Join(t.TempDir(), "nope"), true); !w.Allowed("anything") {
		t.Error("fail-open whitelist should allow even when file missing")
	}
	// 文件不存在 → 全拒。
	if w := newUserWhitelist(filepath.Join(t.TempDir(), "nope"), false); w.Allowed("alice") {
		t.Error("missing file should deny (fail-closed)")
	}
	// 文件为空 → 全拒。
	if w := newUserWhitelist(writeWLFile(t, ""), false); w.Allowed("alice") {
		t.Error("empty file should deny (fail-closed)")
	}
	// 全注释 → 全拒。
	if w := newUserWhitelist(writeWLFile(t, "# only comments\n# another\n"), false); w.Allowed("alice") {
		t.Error("all-comment file should deny (fail-closed)")
	}
}

func TestWhitelistHotReload(t *testing.T) {
	path := writeWLFile(t, "alice\n")
	w := newUserWhitelist(path, false)
	if !w.Allowed("alice") {
		t.Fatal("alice should be allowed initially")
	}
	if w.Allowed("bob") {
		t.Fatal("bob should be denied initially")
	}
	// 改文件（新增用户）→ 无需重启即生效。
	if err := os.WriteFile(path, []byte("alice\nbob\n"), 0o600); err != nil {
		t.Fatalf("rewrite whitelist: %v", err)
	}
	bumpMtime(t, path, time.Second)
	if !w.Allowed("bob") {
		t.Error("bob should be allowed after hot reload (no restart)")
	}
	// 删除用户 → 同样即时生效。
	if err := os.WriteFile(path, []byte("bob\n"), 0o600); err != nil {
		t.Fatalf("rewrite whitelist: %v", err)
	}
	bumpMtime(t, path, 2*time.Second)
	if w.Allowed("alice") {
		t.Error("alice should be denied after removal (hot reload)")
	}
}

func TestWhitelistFailStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web_users")
	if err := os.WriteFile(path, []byte("alice\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w := newUserWhitelist(path, false)
	if !w.Allowed("alice") {
		t.Fatal("alice should be allowed initially")
	}

	// 把文件替换为目录：stat 成功但读取失败 → fail-stale，仍用旧快照裁决。
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !w.Allowed("alice") {
		t.Error("fail-stale: alice should still be allowed from last good snapshot")
	}
	if w.Allowed("bob") {
		t.Error("fail-stale: bob should still be denied")
	}

	// 恢复为正常文件 → 重载成功。
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	if err := os.WriteFile(path, []byte("alice\nbob\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	bumpMtime(t, path, time.Second)
	if !w.Allowed("bob") {
		t.Error("after recovery, bob should be allowed")
	}
}

func TestWhitelistLoadOnce(t *testing.T) {
	// 未配置 / fail-open：LoadOnce 无错误。
	if err := newUserWhitelist("", false).LoadOnce(); err != nil {
		t.Errorf("unconfigured LoadOnce: %v", err)
	}
	if err := newUserWhitelist("", true).LoadOnce(); err != nil {
		t.Errorf("fail-open LoadOnce: %v", err)
	}
	// 正常文件：无错误。
	if err := newUserWhitelist(writeWLFile(t, "alice\n"), false).LoadOnce(); err != nil {
		t.Errorf("valid LoadOnce: %v", err)
	}
	// 文件缺失：返回错误。
	if err := newUserWhitelist(filepath.Join(t.TempDir(), "nope"), false).LoadOnce(); err == nil {
		t.Error("missing-file LoadOnce should return error")
	}
}

func TestWhitelistConcurrent(t *testing.T) {
	path := writeWLFile(t, "alice\n")
	w := newUserWhitelist(path, false)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				_ = w.Allowed("alice")
				_ = w.Allowed("bob")
				if i%2 == 0 && j%40 == 0 {
					_ = os.WriteFile(path, []byte("alice\nbob\n"), 0o600)
					_ = os.WriteFile(path, []byte("alice\n"), 0o600)
				}
				_ = w.LoadOnce()
			}
		}(i)
	}
	wg.Wait()
}
