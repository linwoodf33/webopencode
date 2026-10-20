package audit

import (
	"bytes"
	"encoding/json"
	"html"
)

// 离线回放页占位符 token：统一以 __WS_ 为前缀，便于一次性全量替换并做残留自检。
const (
	tokenOfflineCSS      = "__WS_CSS__"
	tokenOfflinePlayerJS = "__WS_PLAYER_JS__"
	tokenOfflineTitle    = "__WS_TITLE__"
	tokenOfflineMetaJSON = "__WS_META_JSON__"
	tokenOfflineCastJSON = "__WS_CAST_JSON__"
)

// offlineSkeleton 是离线回放页骨架（简体中文文案）。
//
// 大块资源（css/js）由 OfflinePlayerTemplate 内联替换，标题/元信息/cast 三个小 token
// 由 FillOfflinePlayer 填充。骨架自带页头与元信息条的少量基础样式，随后拼接 vendored
// asciinema-player 的 css。
//
// XSS 防线：元信息一律经 textContent 渲染（禁 innerHTML）；title 走 html.EscapeString；
// meta/cast 以 json.Marshal 输出 JS 字面量，Go 默认 HTML 转义会把 < > & 转为 \u003c 等，
// 从而不会在 <script> 上下文内闭合标签。
const offlineSkeleton = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>会话回放 - __WS_TITLE__</title>
<style>
body {
  margin: 0;
  padding: 0;
  background: #121314;
  color: #dcdcdc;
  font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
}
.page-head {
  padding: 12px 16px;
  border-bottom: 1px solid #2b2d2e;
}
.page-head h1 {
  margin: 0 0 8px;
  font-size: 16px;
  font-weight: 600;
}
.meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 18px;
  font-size: 12px;
  color: #9a9a9a;
}
.meta .fld label {
  color: #6f7374;
}
.meta .fld .val {
  color: #dcdcdc;
}
.player {
  width: 100%;
  min-height: 240px;
  max-width: 1000px;
  margin: 16px auto;
  box-sizing: border-box;
  background: #000;
  border: 1px solid #2b2d2e;
  border-radius: 6px;
  overflow: auto;
  padding: 8px;
}
#err {
  display: none;
  margin: 16px;
  padding: 10px 12px;
  border: 1px solid #7a2b2b;
  background: #2a1616;
  color: #ff9c9c;
  font-size: 13px;
}
__WS_CSS__
</style>
</head>
<body>
<header class="page-head">
  <h1>会话回放</h1>
  <div class="meta">
    <span class="fld"><label>用户</label> <span class="val" id="f-username">—</span></span>
    <span class="fld"><label>会话</label> <span class="val" id="f-session">—</span></span>
    <span class="fld"><label>模式</label> <span class="val" id="f-mode">—</span></span>
    <span class="fld"><label>开始</label> <span class="val" id="f-started">—</span></span>
    <span class="fld"><label>结束</label> <span class="val" id="f-ended">—</span></span>
    <span class="fld"><label>时长</label> <span class="val" id="f-duration">—</span></span>
    <span class="fld"><label>来源 IP</label> <span class="val" id="f-ip">—</span></span>
    <span class="fld"><label>结束原因</label> <span class="val" id="f-end-reason">—</span></span>
  </div>
</header>
<div class="player" id="player"></div>
<div id="err">回放器初始化失败</div>
<script>
__WS_PLAYER_JS__
const SESSION_META = __WS_META_JSON__;
const CAST_DATA = __WS_CAST_JSON__;
(function () {
  function setText(id, v) {
    var el = document.getElementById(id);
    if (!el) return;
    el.textContent = (v === null || v === undefined || v === "") ? "—" : String(v);
  }
  function fmt(t) {
    if (t === null || t === undefined || t === "") return null;
    var d = new Date(t);
    if (isNaN(d.getTime())) return null;
    return d.toLocaleString();
  }
  function durationText() {
    var s = new Date(SESSION_META.started_at);
    var e = new Date(SESSION_META.ended_at);
    if (isNaN(s.getTime()) || isNaN(e.getTime())) return null;
    var sec = Math.max(0, Math.round((e.getTime() - s.getTime()) / 1000));
    var m = Math.floor(sec / 60);
    var r = sec % 60;
    return m + " 分 " + r + " 秒";
  }
  var M = SESSION_META;
  setText("f-username", M.username);
  setText("f-session", M.session_id);
  setText("f-mode", M.mode);
  setText("f-started", fmt(M.started_at));
  setText("f-ended", fmt(M.ended_at));
  setText("f-duration", durationText());
  setText("f-ip", M.client ? M.client.ip : null);
  setText("f-end-reason", M.end_reason);
  try {
    AsciinemaPlayer.create({ data: CAST_DATA }, document.getElementById("player"), {
      autoPlay: false,
      controls: true,
      speed: 1
    });
  } catch (e) {
    var errEl = document.getElementById("err");
    if (errEl) errEl.style.display = "block";
  }
})();
</script>
</body>
</html>
`

// OfflinePlayerTemplate 用 js/css 组装一次性离线页骨架，返回含占位符的模板。
//
// 仅内联大块资源（css/js），标题/元信息/cast 仍以 __WS_*__ 占位，交由 FillOfflinePlayer
// 在下载时按会话数据填充。assets 为 nil 时按空内容内联（不 panic），保证降级路径安全。
func OfflinePlayerTemplate(playerJS, playerCSS []byte) []byte {
	tmpl := []byte(offlineSkeleton)
	// 先替换大块（css/js），再做小 token（由 FillOfflinePlayer 完成）。
	tmpl = bytes.ReplaceAll(tmpl, []byte(tokenOfflineCSS), playerCSS)
	tmpl = bytes.ReplaceAll(tmpl, []byte(tokenOfflinePlayerJS), playerJS)
	return tmpl
}

// FillOfflinePlayer 把 meta/cast/title 填充进模板，返回完整离线 HTML。
//
// 替换顺序：大块资源已在 OfflinePlayerTemplate 内联，这里只处理 title/meta/cast 三个
// 小 token。title 走 html.EscapeString；meta/cast 经 json.Marshal 生成 JS 字面量
//（Go 默认 HTML 转义，避免 <script> 上下文被闭合）。返回结果不含任何 __WS_ 残留 token。
func FillOfflinePlayer(tmpl []byte, title string, meta SessionMeta, cast []byte) []byte {
	out := tmpl
	out = bytes.ReplaceAll(out, []byte(tokenOfflineTitle), []byte(html.EscapeString(title)))

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		metaJSON = []byte("{}")
	}
	out = bytes.ReplaceAll(out, []byte(tokenOfflineMetaJSON), metaJSON)

	castJSON, err := json.Marshal(string(cast))
	if err != nil {
		castJSON = []byte(`""`)
	}
	out = bytes.ReplaceAll(out, []byte(tokenOfflineCastJSON), castJSON)
	return out
}
