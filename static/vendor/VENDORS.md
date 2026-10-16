# static/vendor 第三方前端依赖清单
本目录文件均自托管内嵌（//go:embed static），禁止 CDN。CSP script-src 'self' 约束下 font/css 走 'self'。

## asciinema-player（终端录屏回放）
- 版本：3.17.0（npm 包最新稳定 3.x，registry.npmjs.org）
- 文件：asciinema-player.js、asciinema-player.css（dist 拷贝，已去除 sourceMappingURL 注释）
- 获取：npm pack asciinema-player@3.17.0 → dist/bundle/asciinema-player.min.js（改名为 asciinema-player.js）、dist/bundle/asciinema-player.css
- SHA-256：
  - asciinema-player.js: A13C37632E1B5C49FE9128417B9319A9B5BC64CB457DD5AE52CBBA8A3ACEB880
  - asciinema-player.css: F619FE17597043564F03B2C6918B3DAF890EE8B912FB408542FBA11AFADE4FDB
- 说明：v3 API AsciinemaPlayer.create(src, el, opts)。协议选毕（本项目合法使用第三方开源组件）。dist 的 min.js 与 css 本身不含 //# sourceMappingURL 注释（0 处），复制后与官方 tarball 逐字节一致。

## xterm.js（终端前端渲染）
- 版本：xterm.js 5.5.0（npm 包 @xterm/xterm@5.5.0，据 repository 的 DESIGN_SESSIONS.md:480 记录）；addon-fit.js 对应 @xterm/addon-fit@0.10.0
- 文件：xterm.js、xterm.css、addon-fit.js
- SHA-256：
  - xterm.js: 6236F1C9B770148D48C72F90404AB71DADB6CDD731BA7FF9225209FC005B08C4
  - xterm.css: BA8E6985669488981CCF40C0CEFE3ABA80722CB6C92DE7AD628B0BD717FAF2B6
  - addon-fit.js: 03E86DC1BC6B5028071ACAF10060688C25860EE11DC3DE80091730A93413F441

## 获取方式说明
- asciinema-player：在临时目录执行 `npm pack asciinema-player@3.17.0`，解包后取 `package/dist/bundle/asciinema-player.min.js`（复制为 `asciinema-player.js`）与 `package/dist/bundle/asciinema-player.css`，最终均去掉可能存在的 sourceMappingURL 注释尾行，再 `Get-FileHash -Algorithm SHA256` 复核后复制进本目录。
- xterm：`npm pack @xterm/xterm@5.5.0` 取 `lib/xterm.js`、`lib/xterm.css`；`npm pack @xterm/addon-fit@0.10.0` 取 `lib/addon-fit.js`。复制前删除 JS 末尾的 `//# sourceMappingURL=...` 行（xterm.js、addon-fit.js 官方文件各有一条，仓库版本已删除）；xterm.css 无该注释。端口以本清单 SHA-256 为准校验。
