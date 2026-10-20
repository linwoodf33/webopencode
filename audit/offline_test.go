package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// mkOfflineTemplate 构造一个含 js/css 特征串的模板，便于断言内联生效。
func mkOfflineTemplate() []byte {
	return OfflinePlayerTemplate([]byte("/*js-marker*/"), []byte("/*css-marker*/"))
}

// 模板内联 css/js，且含 player 容器；小 token 保留待填。
func TestOfflinePlayerTemplateContainsAssets(t *testing.T) {
	tmpl := mkOfflineTemplate()
	if !bytes.Contains(tmpl, []byte("css-marker")) {
		t.Fatal("css asset not inlined")
	}
	if !bytes.Contains(tmpl, []byte("js-marker")) {
		t.Fatal("js asset not inlined")
	}
	if !bytes.Contains(tmpl, []byte(`id="player"`)) {
		t.Fatal("player container missing")
	}
	// 小 token 在模板阶段应仍保留。
	if !bytes.Contains(tmpl, []byte(tokenOfflineTitle)) {
		t.Fatal("title token should remain in template")
	}
}

// nil assets 不 panic，且仍返回非空骨架。
func TestOfflinePlayerTemplateNilAssetsNoPanic(t *testing.T) {
	var js, css []byte
	tmpl := OfflinePlayerTemplate(js, css)
	if len(tmpl) == 0 {
		t.Fatal("template should be non-empty with nil assets")
	}
	if !bytes.Contains(tmpl, []byte(`id="player"`)) {
		t.Fatal("skeleton should still contain player container")
	}
}

// 填充后不含任何 __WS_ 残留 token。
func TestFillOfflinePlayerNoResidualToken(t *testing.T) {
	meta := SessionMeta{
		SessionID: "s1", Username: "alice", Mode: "bash",
		CreatedAt: time.Unix(1700000000, 0), StartedAt: time.Unix(1700000000, 0),
	}
	out := FillOfflinePlayer(mkOfflineTemplate(), "s1", meta, []byte("{\"version\":2}\n"))
	if bytes.Contains(out, []byte("__WS_")) {
		t.Fatalf("residual token found in output: %q", out)
	}
}

// cast 含 </script> 注入时，输出以 \u003c/script 转义，且 </script> 计数不增加。
func TestFillOfflinePlayerCastScriptEscape(t *testing.T) {
	tmpl := mkOfflineTemplate()
	base := bytes.Count(tmpl, []byte("</script>"))

	evil := "</script><script>alert(1)</script>"
	meta := SessionMeta{SessionID: "s1", Username: "alice"}
	out := FillOfflinePlayer(tmpl, "s1", meta, []byte(evil))

	if !bytes.Contains(out, []byte(`\u003c/script`)) {
		t.Fatal("escaped </script not found in output")
	}
	if got := bytes.Count(out, []byte("</script>")); got != base {
		t.Fatalf("</script> count changed: base=%d got=%d (new closing tag injected)", base, got)
	}
}

// meta.Username 含 </script> 时，JSON 字面量转义为 \u003c/script\u003e。
func TestFillOfflinePlayerMetaScriptEscape(t *testing.T) {
	meta := SessionMeta{SessionID: "s1", Username: "</script>"}
	out := FillOfflinePlayer(mkOfflineTemplate(), "s1", meta, []byte("x"))
	if !bytes.Contains(out, []byte(`\u003c/script\u003e`)) {
		t.Fatal("meta username </script> not escaped in JSON literal")
	}
}

// title 的 HTML 特殊字符被转义。
func TestFillOfflinePlayerTitleEscape(t *testing.T) {
	meta := SessionMeta{SessionID: "s1", Username: "alice"}
	out := FillOfflinePlayer(mkOfflineTemplate(), `<>&"`, meta, []byte("x"))
	if !bytes.Contains(out, []byte("&lt;&gt;&amp;")) {
		t.Fatalf("title not escaped: %q", out)
	}
}

// 空 cast 填充为 `const CAST_DATA = "";`。
func TestFillOfflinePlayerEmptyCast(t *testing.T) {
	meta := SessionMeta{SessionID: "s1", Username: "alice"}
	out := FillOfflinePlayer(mkOfflineTemplate(), "s1", meta, nil)
	if !bytes.Contains(out, []byte(`const CAST_DATA = "";`)) {
		t.Fatalf("empty cast literal wrong: %q", out)
	}
}

// 超长 cast（100KB）完整保留。
func TestFillOfflinePlayerLargeCast(t *testing.T) {
	cast := strings.Repeat("A", 100*1024) + "-tail-marker"
	meta := SessionMeta{SessionID: "s1", Username: "alice"}
	out := FillOfflinePlayer(mkOfflineTemplate(), "s1", meta, []byte(cast))
	if !bytes.Contains(out, []byte("A"+strings.Repeat("A", 100*1024-1))) {
		t.Fatal("large cast body not preserved")
	}
	if !bytes.Contains(out, []byte("-tail-marker")) {
		t.Fatal("large cast tail marker missing")
	}
}
