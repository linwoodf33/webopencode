// Web Shell 前端逻辑
// 负责：登录（POST /api/login 获取 mgmt_token）、会话管理（列表/新建/进入/延期/关闭）、
//       WebSocket 连接（ticket 换发）、xterm 事件绑定、消息编解码、文件上传。
//
// 凭证模型（DESIGN_SESSIONS §4）：
//   - mgmt_token：登录后签发的用户级管理凭证，Authorization: Bearer，用于所有 HTTP 会话级操作。
//   - ticket：由 mgmt_token 换取的一次性短时凭证，仅用于建立一次 WebSocket（/ws?ticket=）。
//   - sessionID：会话标识（非机密），不再单独作为接入凭证。

(function () {
  "use strict";

  // 检查 xterm 是否加载成功（自托管文件缺失时给出可读提示）。
  // 安全：使用 createElement/textContent 构建提示，禁止 innerHTML 拼接。
  if (typeof Terminal === "undefined" || typeof FitAddon === "undefined") {
    var errBox = document.createElement("p");
    errBox.className = "xterm-load-error";
    errBox.textContent =
      "Failed to load xterm.js. Please check that /static/vendor/ is available.";
    document.body.appendChild(errBox);
    return;
  }

  var loginPanel = document.getElementById("login-panel");
  var loginForm = document.getElementById("login-form");
  var usernameInput = document.getElementById("username");
  var passwordInput = document.getElementById("password");
  var loginError = document.getElementById("login-error");
  var terminalEl = document.getElementById("terminal");

  var sessionPanel = document.getElementById("session-panel");
  var sessionUserEl = document.getElementById("session-user");
  var sessionCountsEl = document.getElementById("session-counts");
  var sessionList = document.getElementById("session-list");
  var sessionEmpty = document.getElementById("session-empty");
  var btnLogout = document.getElementById("btn-logout");
  var newTypeLong = document.getElementById("new-type-long");
  var newTypeShort = document.getElementById("new-type-short");
  var newHoursWrap = document.getElementById("new-hours-wrap");
  var newHours = document.getElementById("new-hours");
  var newModeBash = document.getElementById("new-mode-bash");
  var newModeOpencode = document.getElementById("new-mode-opencode");
  var newModeCodex = document.getElementById("new-mode-codex");
  var newError = document.getElementById("new-error");

  var termWrap = document.getElementById("term-wrap");
  var btnSessions = document.getElementById("btn-sessions");
  var btnFontMinus = document.getElementById("btn-font-minus");
  var btnFontPlus = document.getElementById("btn-font-plus");
  var fontSizeLabel = document.getElementById("font-size-label");
  var themeSelect = document.getElementById("theme-select");
  var btnUpload = document.getElementById("btn-upload");
  var fileInput = document.getElementById("file-input");

  // div 弹窗（消息 / 上传进度）相关元素。
  var modalOverlay = document.getElementById("modal-overlay");
  var modalMessage = document.getElementById("modal-message");
  var modalOk = document.getElementById("modal-ok");
  var uploadOverlay = document.getElementById("upload-overlay");
  var uploadTitle = document.getElementById("upload-title");
  var uploadFilename = document.getElementById("upload-filename");
  var uploadBar = document.getElementById("upload-bar");
  var uploadPercent = document.getElementById("upload-percent");
  var uploadCancel = document.getElementById("upload-cancel");
  var activeUploadXhr = null; // 当前进行中的上传请求（用于取消）

  // API Key 输入弹窗相关元素（opencode 启动前补全 api_key）。
  var authOverlay = document.getElementById("auth-overlay");
  var authFields = document.getElementById("auth-fields");
  var authOkBtn = document.getElementById("auth-ok-btn");
  var authCancelBtn = document.getElementById("auth-cancel-btn");
  // auth 弹窗打开期间置 true，暂停终端输入转发（此时 opencode 尚未启动）。
  var authModalOpen = false;

  var modePanel = document.getElementById("mode-panel");
  var welcomeEl = document.getElementById("toolbar-welcome");

  // 审计 / 回放相关元素。
  var btnAudit = document.getElementById("btn-audit");
  var auditPanel = document.getElementById("audit-panel");
  var auditUserFilter = document.getElementById("audit-user-filter");
  var auditUsernameFilter = document.getElementById("audit-username-filter");
  var auditFilterBtn = document.getElementById("audit-filter-btn");
  var auditList = document.getElementById("audit-list");
  var auditEmpty = document.getElementById("audit-empty");
  var btnAuditBack = document.getElementById("btn-audit-back");
  var playbackPanel = document.getElementById("playback-panel");
  var playbackTitle = document.getElementById("playback-title");
  var btnPlaybackBack = document.getElementById("btn-playback-back");
  var showInputWrap = document.getElementById("show-input-wrap");
  var showInputToggle = document.getElementById("show-input-toggle");
  var playbackEl = document.getElementById("playback");
  var inputPanel = document.getElementById("input-panel");
  var recordingNotice = document.getElementById("recording-notice");
  var recordingNoticeOk = document.getElementById("recording-notice-ok");

  // 审计列表：日期间隔筛选 / 分页 / 单页行数。
  var auditDateFrom = document.getElementById("audit-date-from");
  var auditDateTo = document.getElementById("audit-date-to");
  var auditDateFilterBtn = document.getElementById("audit-date-filter-btn");
  var auditDateResetBtn = document.getElementById("audit-date-reset-btn");
  var auditPageSizeSel = document.getElementById("audit-page-size");
  var auditPrevBtn = document.getElementById("audit-prev-btn");
  var auditNextBtn = document.getElementById("audit-next-btn");
  var auditPageInfo = document.getElementById("audit-page-info");

  var term = null;
  var fitAddon = null;
  var ws = null;
  var closed = false;
  // 防重入：进入终端的三个入口共用，ticket 请求期间置位，回调后复位。
  var entering = false;

  // window.resize 监听只注册一次（模块级 flag），避免多次进出终端时重复累积。
  var resizeListenerAdded = false;

  // 右键复制监听已安装标志（terminalEl 为常驻元素，仅需一次）。
  var contextmenuCopyInstalled = false;

  // 当前会话标识（用于页面刷新后恢复同一终端），以及管理凭证/用户名。
  var currentSessionID = null;
  var mgmtToken = null;
  var currentUser = null;
  var currentMode = "bash";

  // 回放状态：当前打开的审计条目、asciinema-player 实例、已加载的 cast 文本。
  var currentPlaybackItem = null;
  var playerInstance = null;
  var playerScriptLoaded = false;   // asciinema-player.js 是否已注入并加载完成
  var playerScriptLoading = false;  // 是否正在注入（防并发重复注入）
  var playerLoadCbs = [];           // 注入期间排队的回调

  // 服务端下发的会话配置（登录响应提供），用于面板默认值与上限校验。
  var cfg = {
    opencodeEnabled: false,
    codexEnabled: false,
    sessionTtlHours: 24,
    maxSessionTtlHours: 168,
    maxLongSessions: 3,
    maxTotalSessions: 10,
    auditEnabled: false,   // /api/login 的 audit_enabled：是否启用录制
    isAuditor: false,      // /api/login 的 is_auditor：是否审计员
    userCanViewSelf: false,      // /api/login 的 user_can_view_self：普通用户可否查看本人录屏
  };

  // 录制明示「已读」标记（sessionStorage，本标签页内有效）。
  var AUDIT_NOTICE_KEY = "webshell.audit.notice_dismissed";

  // 审计列表偏好（localStorage：记住上次日期间隔与单页行数）。
  var AUDIT_PREFS_KEY = "webshell.auditPrefs";
  var AUDIT_PAGE_SIZES = [10, 20, 30, 50];

  // 审计列表分页 / 筛选状态。
  var auditPage = 1;
  var auditPageSize = 10;
  var auditFromDate = "";
  var auditToDate = "";
  var auditTotal = 0;

  var MGMT_STORAGE_KEY = "webshell.mgmt";
  var SESSION_STORAGE_KEY = "webshell.session";

  // ===== 存储（sessionStorage：本标签页内有效，关闭标签即清除）=====

  // 保存 mgmt_token 与登录配置，供页面刷新后免重登直接实时列表。
  function saveMgmt(token, username, data) {
    if (!token) return;
    try {
      sessionStorage.setItem(
        MGMT_STORAGE_KEY,
        JSON.stringify({
          token: token,
          username: username,
          opencode_enabled: !!data.opencode_enabled,
          codex_enabled: !!data.codex_enabled,
          session_ttl_hours: data.session_ttl_hours,
          max_session_ttl_hours: data.max_session_ttl_hours,
          max_long_sessions: data.max_long_sessions,
          max_total_sessions: data.max_total_sessions,
          is_auditor: !!data.is_auditor,
          audit_enabled: !!data.audit_enabled,
          user_can_view_self: !!data.user_can_view_self,   // 普通用户可查看本人录屏，刷新后恢复
        })
      );
    } catch (e) {}
  }

  function loadMgmt() {
    try {
      var raw = sessionStorage.getItem(MGMT_STORAGE_KEY);
      if (!raw) return null;
      var o = JSON.parse(raw);
      if (o && o.token) return o;
    } catch (e) {}
    return null;
  }

  function clearMgmt() {
    mgmtToken = null;
    try { sessionStorage.removeItem(MGMT_STORAGE_KEY); } catch (e) {}
  }

  // 应用登录/恢复得到的配置（缺失时保留默认值）。
  function applyConfig(src) {
    if (!src) return;
    if (typeof src.opencode_enabled === "boolean") cfg.opencodeEnabled = src.opencode_enabled;
    if (typeof src.codex_enabled === "boolean") cfg.codexEnabled = src.codex_enabled;
    var v;
    v = parseInt(src.session_ttl_hours, 10);
    if (!isNaN(v) && v > 0) cfg.sessionTtlHours = v;
    v = parseInt(src.max_session_ttl_hours, 10);
    if (!isNaN(v) && v > 0) cfg.maxSessionTtlHours = v;
    v = parseInt(src.max_long_sessions, 10);
    if (!isNaN(v) && v > 0) cfg.maxLongSessions = v;
    v = parseInt(src.max_total_sessions, 10);
    if (!isNaN(v) && v > 0) cfg.maxTotalSessions = v;
    if (typeof src.audit_enabled === "boolean") cfg.auditEnabled = src.audit_enabled;
    if (typeof src.is_auditor === "boolean") cfg.isAuditor = src.is_auditor;
    if (typeof src.user_can_view_self === "boolean") cfg.userCanViewSelf = src.user_can_view_self;
  }

  // 会话持久化：把 sessionID/mode 存入 sessionStorage，页面刷新后据此恢复同一终端。
  function saveSession(sid, mode) {
    if (!sid) return;
    try {
      sessionStorage.setItem(
        SESSION_STORAGE_KEY,
        JSON.stringify({ sid: sid, mode: mode || "bash" })
      );
    } catch (e) {}
  }

  function loadSession() {
    try {
      var raw = sessionStorage.getItem(SESSION_STORAGE_KEY);
      if (!raw) return null;
      var o = JSON.parse(raw);
      if (o && o.sid) return o;
    } catch (e) {}
    return null;
  }

  function clearSession() {
    currentSessionID = null;
    try { sessionStorage.removeItem(SESSION_STORAGE_KEY); } catch (e) {}
  }

  // ===== HTTP 辅助 =====

  // 带 mgmt Authorization 的 fetch；自动 ATTACH MGMT 头。
  function apiFetch(path, opts) {
    opts = opts || {};
    var headers = opts.headers || {};
    if (mgmtToken) headers["Authorization"] = "Bearer " + mgmtToken;
    opts.headers = headers;
    return fetch(path, opts);
  }

  // 发 JSON 请求并解析响应，返回 {ok, status, data}。
  function apiJSON(path, opts) {
    return apiFetch(path, opts).then(function (resp) {
      return resp.json().then(
        function (data) { return { ok: resp.ok, status: resp.status, data: data }; },
        function () { return { ok: resp.ok, status: resp.status, data: null }; }
      );
    });
  }

  // 后端布尔字段可能为 true / "true"。
  function isTrue(v) {
    return v === true || v === "true";
  }

  // mgmt 过期/失效：清理并回到登录页。
  function handleAuthExpired() {
    clearMgmt();
    clearSession();
    showLogin("登录已过期，请重新登录");
  }

  // ===== 面板切换 =====

  function showOnly(el) {
    var panels = [loginPanel, modePanel, sessionPanel, termWrap, auditPanel, playbackPanel];
    for (var i = 0; i < panels.length; i++) {
      if (panels[i]) panels[i].classList.add("hidden");
    }
    if (el) el.classList.remove("hidden");
  }

  // 终端字号（px），从 localStorage 恢复，默认 14。
  var FONT_STORAGE_KEY = "webshell.fontSize";
  var MIN_FONT = 8;
  var MAX_FONT = 32;
  var FONT_STEP = 2;
  var currentFontSize = (function () {
    var n = parseInt(localStorage.getItem(FONT_STORAGE_KEY), 10);
    return isNaN(n) || n < MIN_FONT || n > MAX_FONT ? 14 : n;
  })();

  // 终端主题定义（xterm.js theme 字段）。
  var THEME_STORAGE_KEY = "webshell.theme";
  var THEMES = {
    "vscode-dark": {
      background: "#1e1e1e", foreground: "#cccccc", cursor: "#aeafad",
      cursorAccent: "#1e1e1e", selectionBackground: "#264f78",
      black: "#000000", red: "#cd3131", green: "#0dbc79", yellow: "#e5e510",
      blue: "#2472c8", magenta: "#bc3fbc", cyan: "#11a8cd", white: "#e5e5e5",
      brightBlack: "#666666", brightRed: "#f14c4c", brightGreen: "#23d18b",
      brightYellow: "#f5f543", brightBlue: "#3b8eea", brightMagenta: "#d670d6",
      brightCyan: "#29b8db", brightWhite: "#e5e5e5",
    },
    dracula: {
      background: "#282a36", foreground: "#f8f8f2", cursor: "#f8f8f2",
      cursorAccent: "#282a36", selectionBackground: "#44475a",
      black: "#21222c", red: "#ff5555", green: "#50fa7b", yellow: "#f1fa8c",
      blue: "#bd93f9", magenta: "#ff79c6", cyan: "#8be9fd", white: "#f8f8f2",
      brightBlack: "#6272a4", brightRed: "#ff6e6e", brightGreen: "#69ff94",
      brightYellow: "#ffffa5", brightBlue: "#d6acff", brightMagenta: "#ff92df",
      brightCyan: "#a4ffff", brightWhite: "#ffffff",
    },
    monokai: {
      background: "#272822", foreground: "#f8f8f2", cursor: "#f8f8f0",
      cursorAccent: "#272822", selectionBackground: "#49483e",
      black: "#272822", red: "#f92672", green: "#a6e22e", yellow: "#f4bf75",
      blue: "#66d9ef", magenta: "#ae81ff", cyan: "#a1efe4", white: "#f8f8f2",
      brightBlack: "#75715e", brightRed: "#f92672", brightGreen: "#a6e22e",
      brightYellow: "#f4bf75", brightBlue: "#66d9ef", brightMagenta: "#ae81ff",
      brightCyan: "#a1efe4", brightWhite: "#f9f8f5",
    },
    nord: {
      background: "#2e3440", foreground: "#d8dee9", cursor: "#d8dee9",
      cursorAccent: "#2e3440", selectionBackground: "#4c566a",
      black: "#3b4252", red: "#bf616a", green: "#a3be8c", yellow: "#ebcb8b",
      blue: "#81a1c1", magenta: "#b48ead", cyan: "#88c0d0", white: "#e5e9f0",
      brightBlack: "#4c566a", brightRed: "#bf616a", brightGreen: "#a3be8c",
      brightYellow: "#ebcb8b", brightBlue: "#81a1c1", brightMagenta: "#b48ead",
      brightCyan: "#8fbcbb", brightWhite: "#eceff4",
    },
    "solarized-dark": {
      background: "#002b36", foreground: "#839496", cursor: "#93a1a1",
      cursorAccent: "#002b36", selectionBackground: "#073642",
      black: "#073642", red: "#dc322f", green: "#859900", yellow: "#b58900",
      blue: "#268bd2", magenta: "#d33682", cyan: "#2aa198", white: "#eee8d5",
      brightBlack: "#586e75", brightRed: "#cb4b16", brightGreen: "#859900",
      brightYellow: "#b58900", brightBlue: "#268bd2", brightMagenta: "#d33682",
      brightCyan: "#2aa198", brightWhite: "#fdf6e3",
    },
    "one-dark": {
      background: "#282c34", foreground: "#abb2bf", cursor: "#528bff",
      cursorAccent: "#282c34", selectionBackground: "#3e4451",
      black: "#282c34", red: "#e06c75", green: "#98c379", yellow: "#e5c07b",
      blue: "#61afef", magenta: "#c678dd", cyan: "#56b6c2", white: "#abb2bf",
      brightBlack: "#5c6370", brightRed: "#e06c75", brightGreen: "#98c379",
      brightYellow: "#e5c07b", brightBlue: "#61afef", brightMagenta: "#c678dd",
      brightCyan: "#56b6c2", brightWhite: "#ffffff",
    },
    "tokyo-night": {
      background: "#1a1b26", foreground: "#a9b1d6", cursor: "#c0caf5",
      cursorAccent: "#1a1b26", selectionBackground: "#28344a",
      black: "#15161e", red: "#f7768e", green: "#9ece6a", yellow: "#e0af68",
      blue: "#7aa2f7", magenta: "#bb9af7", cyan: "#7dcfff", white: "#a9b1d6",
      brightBlack: "#414868", brightRed: "#f7768e", brightGreen: "#9ece6a",
      brightYellow: "#e0af68", brightBlue: "#7aa2f7", brightMagenta: "#bb9af7",
      brightCyan: "#7dcfff", brightWhite: "#c0caf5",
    },
    "github-light": {
      background: "#ffffff", foreground: "#24292e", cursor: "#044289",
      cursorAccent: "#ffffff", selectionBackground: "#c8e1ff",
      black: "#24292e", red: "#d73a49", green: "#22863a", yellow: "#b08800",
      blue: "#005cc5", magenta: "#6f42c1", cyan: "#3192aa", white: "#6a737d",
      brightBlack: "#959da5", brightRed: "#cb2431", brightGreen: "#22863a",
      brightYellow: "#b08800", brightBlue: "#005cc5", brightMagenta: "#6f42c1",
      brightCyan: "#3192aa", brightWhite: "#6a737d",
    },
  };
  // 当前主题，从 localStorage 恢复，默认 VS Code Dark。
  var currentTheme = (function () {
    var t = localStorage.getItem(THEME_STORAGE_KEY);
    return THEMES[t] ? t : "vscode-dark";
  })();

  // 发送 resize 消息到服务端。
  function sendResize() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    if (!term) return;
    var dims = { cols: term.cols, rows: term.rows };
    ws.send(JSON.stringify({ type: "resize", data: dims }));
  }

  var ATTACH_RESIZE_JITTER_MS = 150;   // 抖动后还原的延时
  var isAttachEntry = false;           // 本次是否为 re-attach 进入（create 不抖动）

  // 发送一次带行数偏移的 resize（guard 语义同 sendResize）。
  function sendResizeWithRows(rowOffset) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    if (!term) return;
    var rows = term.rows + rowOffset;
    if (rows < 1) rows = term.rows + 1;   // 边界：rows==1 时减一无效，改 +1，保证 ≥1 且 ≠ 原值
    ws.send(JSON.stringify({ type: "resize", data: { cols: term.cols, rows: rows } }));
  }

  // re-attach 专用：抖动（rows-1）→ 延时 → 还原真实尺寸，使前台 TUI 经历两次“净不同”变化而重绘。
  function jitterResizeRedraw() {
    if (!isAttachEntry) return;
    sendResizeWithRows(-1);
    setTimeout(function () {
      if (ws && ws.readyState === WebSocket.OPEN) sendResize();  // 还原真实尺寸
      isAttachEntry = false;                                     // 一次性
    }, ATTACH_RESIZE_JITTER_MS);
  }

  // 通过 WebSocket 向终端发送一次回车（\r），用于上传成功/取消后让终端
  // 回到 shell 交互提示符界面。
  function sendEnter() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: "input", data: "\r" }));
  }

  // 建立 WebSocket 连接：仅携带一次性 ticket（60s）。
  // 新建与接入均先经 /api/session/ticket 换取 ticket，再走本函数。
  function connect(ticket) {
    // 关闭上一条连接：旧连接的回调会因代次守卫而全部失效。
    if (ws) {
      try { ws.close(); } catch (e) {}
    }
    closed = false;
    var proto = location.protocol === "https:" ? "wss://" : "ws://";
    var url = proto + location.host + "/ws?ticket=" + encodeURIComponent(ticket);
    // 捕获本条连接引用，并以此判定回调是否仍属于「当前」连接。
    var myWs = new WebSocket(url);
    ws = myWs;

    function isCurrent() {
      return ws === myWs;
    }

    myWs.onopen = function () {
      if (!isCurrent()) return;
      term.writeln("\x1b[32m[connected]\x1b[0m");
      // 重连状态重同步：服务端 TUI 仍认为处于备用缓冲（1049h 随旧页面丢失），
      // 其开启的鼠标上报模式（1000/1002/1003/1006）同样随旧页面丢失。
      // 先补齐缓冲对齐与鼠标模式，再由 jitterResizeRedraw 触发 TUI 全量重绘。
      if (isAttachEntry && currentMode !== "bash") {
        term.write("\x1b[?1049h");
        term.write("\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h");
      }
      sendResize();
      jitterResizeRedraw();   // 新增
    };

    myWs.onmessage = function (evt) {
      if (!isCurrent()) return;
      var msg;
      try {
        msg = JSON.parse(evt.data);
      } catch (e) {
        return;
      }

      if (msg.type === "output") {
        // 服务端将 pty 输出字节 base64 编码为字符串，这里解码回
        // Uint8Array 后再交给 xterm 渲染，确保控制字符不被破坏。
        try {
          var bin = atob(msg.data);
          var bytes = new Uint8Array(bin.length);
          for (var i = 0; i < bin.length; i++) {
            bytes[i] = bin.charCodeAt(i);
          }
          term.write(bytes);
        } catch (e) {
          // 解码失败：丢弃该帧，不中断会话。
        }
      } else if (msg.type === "session") {
        // 服务端下发会话标识：保存到 sessionStorage，供刷新后恢复同一终端。
        var sid = (msg.data && msg.data.id) || "";
        if (sid) {
          currentSessionID = sid;
          saveSession(sid, currentMode);
        }
      } else if (msg.type === "auth-request") {
        // opencode 启动前服务端请求补全缺失的 api_key：弹出输入框。
        showAuthModal(msg.data);
      } else if (msg.type === "close") {
        var reason = (msg.data && msg.data.reason) || "unknown";
        if (reason === "replaced by new connection") {
          // 接管语义：本连接已被新连接替换，静默收尾，不当作「会话结束」。
          closed = true;
          if (ws) {
            try { ws.close(); } catch (e) {}
            ws = null;
          }
          if (term) {
            try { term.dispose(); } catch (e) {}
            term = null;
          }
          clearSession();
          if (mgmtToken) {
            showSessionPanel(false);
          } else {
            showLogin("会话已在其他窗口打开");
          }
          return;
        }
        // 真实结束（shell exited / expired / closed / not available / startup 失败等）。
        term.writeln("\r\n\x1b[31m[closed: " + reason + "]\x1b[0m");
        closed = true;
        if (ws) {
          try { ws.close(); } catch (e) {}
          ws = null;
        }
        if (term) {
          try { term.dispose(); } catch (e) {}
          term = null;
        }
        clearSession();
        // mgmt 仍有效时回到会话列表，否则回登录页。
        if (mgmtToken) {
          showSessionPanel(false);
          showModal("会话已结束：" + reason);
        } else {
          showLogin("会话已结束：" + reason);
        }
      }
    };

    myWs.onerror = function () {
      if (!isCurrent()) return;
      if (term) term.writeln("\r\n\x1b[31m[websocket error]\x1b[0m");
    };

    myWs.onclose = function () {
      if (!isCurrent()) return;
      ws = null;
      if (!closed && term) {
        term.writeln("\r\n\x1b[31m[connection lost]\x1b[0m");
      }
    };
  }

  // 创建终端并挂载。
  function createTerminal() {
    if (term) {
      try { term.dispose(); } catch (e) {}
      term = null;
    }
    term = new Terminal({
      convertEol: true, // 将 CRLF 转为 LF
      cursorBlink: true, // 光标闪烁
      fontSize: currentFontSize,
      theme: THEMES[currentTheme],
      allowProposedApi: true, // 启用 parser API 以便注册 OSC 52 剪贴板
    });

    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(terminalEl);
    fitAddon.fit();
    installOsc52Clipboard(term);
    fontSizeLabel.textContent = currentFontSize + "px";
    themeSelect.value = currentTheme;

    // 选中后右键=复制：终端有选区时右键复制并阻止浏览器默认菜单；
    // 无选区则保留默认菜单（浏览器自带复制/粘贴等）。
    if (!contextmenuCopyInstalled) {
      contextmenuCopyInstalled = true;
      terminalEl.addEventListener("contextmenu", function (ev) {
        if (term && term.hasSelection && term.getSelection()) {
          ev.preventDefault();
          copyToClipboardBestEffort(term.getSelection());
        }
      });
    }

    // 用户输入 -> 发送 input 消息。
    term.onData(function (data) {
      if (authModalOpen) return;
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "input", data: data }));
      }
    });

    term.onResize(function () {
      sendResize();
    });

    // window.resize 处理器只注册一次：回调在触发时读取模块级 fitAddon，
    // 因此始终作用于当前终端；避免每次 createTerminal 重复 addEventListener 累积。
    if (!resizeListenerAdded) {
      resizeListenerAdded = true;
      window.addEventListener("resize", function () {
        if (fitAddon) fitAddon.fit();
        sendResize();
      });
    }
  }

  // OSC 52 剪贴板（opencode 等 TUI 复制到系统剪贴板）：ESC]52;Pc;Pd(BEL|ESC\)
  // xterm 默认忽略该序列；这里注册处理器把 base64(Pd) 解码后写入浏览器剪贴板。
  // 站点为 http（非安全上下文）时 navigator.clipboard 不可用，回退隐藏 textarea + execCommand。
  function installOsc52Clipboard(t) {
    if (!t || !t.parser || !t.parser.registerOscHandler) return;
    t.parser.registerOscHandler(52, function (data) {
      // data 是字符串 payload，形如 "c;SGVsbG8=" 或 ";;aGk="（Pc 可省略，省略默认剪贴板）。
      var idx = data.indexOf(";");
      var pc = idx === -1 ? data : data.slice(0, idx);
      var pd = idx === -1 ? "" : data.slice(idx + 1);
      if (pd === "" || pd === "?") return true;   // 读取/查询：忽略，直接消费
      if (pc !== "" && pc.indexOf("c") === -1 && pc.indexOf("p") === -1) return true; // 非剪贴板目标：忽略；空 Pc 视为默认剪贴板
      try {
        var b64 = pd.replace(/\s+/g, "");
        var bin = atob(b64);
        var u8 = new Uint8Array(bin.length);
        for (var i = 0; i < bin.length; i++) u8[i] = bin.charCodeAt(i) & 0xff;
        var text = new TextDecoder("utf-8").decode(u8);
        copyToClipboardBestEffort(text);
      } catch (e) {}
      return true;
    });
  }

  // 尽力复制：安全上下文用异步剪贴板 API，否则隐藏 textarea + execCommand 回退。
  function copyToClipboardBestEffort(text) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text)["catch"](function () { fallbackCopy(text); });
    } else {
      fallbackCopy(text);
    }
  }

  function fallbackCopy(text) {
    try {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.left = "-9999px";
      ta.style.top = "0";
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      ta.setSelectionRange(0, text.length);
      document.execCommand("copy");
      document.body.removeChild(ta);
    } catch (e) {}
  }

  // 设置终端字号并持久化；调整后重新 fit 并同步终端尺寸。
  function applyFontSize(newSize) {
    if (newSize < MIN_FONT) newSize = MIN_FONT;
    if (newSize > MAX_FONT) newSize = MAX_FONT;
    currentFontSize = newSize;
    localStorage.setItem(FONT_STORAGE_KEY, String(newSize));
    fontSizeLabel.textContent = newSize + "px";
    if (term) {
      term.options.fontSize = newSize;
      if (fitAddon) fitAddon.fit();
      sendResize();
    }
  }

  // 设置终端主题并持久化；主题直接反映到已运行的终端上。
  function applyTheme(themeName) {
    if (!THEMES[themeName]) return;
    currentTheme = themeName;
    localStorage.setItem(THEME_STORAGE_KEY, themeName);
    themeSelect.value = themeName;
    if (term) {
      term.options.theme = THEMES[themeName];
    }
  }

  // ===== div 弹窗 =====

  function showModal(message) {
    modalMessage.textContent = message || "";
    modalOverlay.classList.remove("hidden");
    modalOk.focus();
  }

  function hideModal() {
    modalOverlay.classList.add("hidden");
  }

  // ===== API Key 输入弹窗（opencode 启动前补全 api_key）=====

  // 显示 API Key 输入弹窗：根据 data.envVars 动态渲染每行
  // label(envVar) + input[type=password]。
  // 安全：用 createElement/textContent 渲染 envVar，禁止 innerHTML 拼接
  // 用户可控串（防 XSS）。
  function showAuthModal(data) {
    var envVars = (data && data.envVars) || [];
    authFields.innerHTML = ""; // 清空旧输入（此处内容全部由下方代码创建）

    for (var i = 0; i < envVars.length; i++) {
      var envVar = String(envVars[i] || "");

      var field = document.createElement("div");
      field.className = "auth-field";

      var label = document.createElement("label");
      label.textContent = envVar;
      field.appendChild(label);

      var input = document.createElement("input");
      input.type = "password";
      input.autocomplete = "off";
      input.spellcheck = false;
      input.dataset.envVar = envVar;
      field.appendChild(input);

      authFields.appendChild(field);
    }

    if (envVars.length > 0) {
      authOverlay.classList.remove("hidden");
      authModalOpen = true;
      var first = authFields.querySelector("input");
      if (first) first.focus();
    }
  }

  // 隐藏 API Key 输入弹窗并清空输入（防残留 api_key 显示在 DOM 中）。
  function hideAuthModal() {
    authModalOpen = false;
    authOverlay.classList.add("hidden");
    authFields.innerHTML = "";
  }

  // 确定：收集所有输入框的值，逐项回传 auth-response。
  function authOk() {
    var inputs = authFields.querySelectorAll("input");
    var items = [];
    for (var i = 0; i < inputs.length; i++) {
      var val = inputs[i].value.trim();
      if (val === "") {
        showModal("请填写全部 API Key");
        return;
      }
      items.push({ envVar: inputs[i].dataset.envVar || "", apiKey: val });
    }
    if (items.length === 0) {
      hideAuthModal();
      return;
    }
    for (var j = 0; j < items.length; j++) {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({
          type: "auth-response",
          data: { envVar: items[j].envVar, apiKey: items[j].apiKey },
        }));
      }
    }
    hideAuthModal();
  }

  function authCancel() {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "auth-cancel" }));
    }
    hideAuthModal();
  }

  // ===== 上传进度弹窗 =====

  function showUploadProgress(filename) {
    uploadFilename.textContent = filename || "";
    uploadBar.style.width = "0%";
    uploadPercent.textContent = "0%";
    uploadTitle.textContent = "正在上传...";
    uploadOverlay.classList.remove("hidden");
  }

  function setUploadProgress(pct) {
    if (pct < 0) pct = 0;
    if (pct > 100) pct = 100;
    uploadBar.style.width = pct + "%";
    uploadPercent.textContent = pct + "%";
  }

  function hideUploadProgress() {
    uploadOverlay.classList.add("hidden");
  }

  // 上传前先做权限预检（GET /api/upload-check，Bearer mgmt），目标目录不可写时
  // 直接弹窗提示，不启动上传，避免上传完成后写文件才报权限错误。
  function uploadFile(file) {
    if (!currentSessionID) {
      showModal("上传失败：无有效会话");
      return;
    }

    var checkUrl = "/api/upload-check?session=" + encodeURIComponent(currentSessionID);
    apiFetch(checkUrl)
      .then(function (resp) {
        return resp.json().then(function (data) {
          return { ok: resp.ok, data: data };
        });
      })
      .then(function (r) {
        if (r.ok && r.data && isTrue(r.data.ok)) {
          doUpload(file);
        } else {
          var msg = (r.data && r.data.error) || "当前目录不可写";
          showModal(msg);
        }
      })
      .catch(function () {
        showModal("无法检查目录权限，请稍后重试");
      });
  }

  // 实际执行上传（假设已通过权限预检）。目标目录由服务端取会话实时工作目录决定；
  // 使用 XMLHttpRequest 以获取上传进度并展示进度条，支持中途取消。
  function doUpload(file) {
    term.writeln("\r\n\x1b[36m[正在上传 " + file.name + " ...]\x1b[0m");
    showUploadProgress(file.name);

    var fd = new FormData();
    fd.append("file", file);

    var xhr = new XMLHttpRequest();
    var canceled = false;
    activeUploadXhr = xhr;
    xhr.open(
      "POST",
      "/api/upload?session=" + encodeURIComponent(currentSessionID),
      true
    );
    xhr.setRequestHeader("Authorization", "Bearer " + mgmtToken);

    xhr.upload.onprogress = function (e) {
      if (e.lengthComputable) {
        setUploadProgress(Math.round((e.loaded / e.total) * 100));
      }
    };

    xhr.onload = function () {
      if (canceled) return;
      activeUploadXhr = null;
      hideUploadProgress();
      var data = null;
      try {
        data = JSON.parse(xhr.responseText);
      } catch (e) {}
      if (xhr.status === 200 && data && isTrue(data.ok)) {
        term.writeln("\x1b[32m[上传成功：" + (data.path || file.name) + "]\x1b[0m");
        sendEnter();
      } else {
        var msg = (data && data.error) || ("上传失败（HTTP " + xhr.status + "）");
        showModal(msg);
      }
    };

    xhr.onerror = function () {
      if (canceled) return;
      activeUploadXhr = null;
      hideUploadProgress();
      showModal("上传失败：网络错误");
    };

    xhr.ontimeout = function () {
      if (canceled) return;
      activeUploadXhr = null;
      hideUploadProgress();
      showModal("上传失败：请求超时");
    };

    xhr.send(fd);
  }

  function cancelUpload() {
    if (activeUploadXhr) {
      activeUploadXhr.abort();
      activeUploadXhr = null;
      hideUploadProgress();
      term.writeln("\r\n\x1b[33m[上传已取消]\x1b[0m");
      sendEnter();
    }
  }

  // ===== 面板/终端入口 =====

  // 显示登录面板，隐藏终端。会话已结束/已退出，清除会话记录。
  function showLogin(message) {
    clearSession();
    if (term) {
      try { term.dispose(); } catch (e) {}
      term = null;
    }
    if (ws) {
      try { ws.close(); } catch (e) {}
      ws = null;
    }
    if (recordingNotice) recordingNotice.classList.add("hidden");
    showOnly(loginPanel);
    loginError.textContent = message || "";
    usernameInput.value = "";
    passwordInput.value = "";
    usernameInput.focus();
  }

  function setWelcome(username) {
    if (welcomeEl) {
      welcomeEl.textContent = "欢迎使用 Web Shell，";
      if (username) welcomeEl.textContent = "欢迎您，" + username + "！";
    }
  }

  // 进入终端：ticket 已换取，sid 为已知会话（新建为 null，待服务端下发）。
  function enterTerminal(ticket, mode, sid) {
    currentMode = mode || "bash";
    currentSessionID = sid || null;
    isAttachEntry = !!sid;   // re-attach（sid 非空）才抖动重绘；create 不抖动
    if (sid) {
      saveSession(sid, currentMode);
    } else {
      clearSession();
    }
    setWelcome(currentUser);
    showOnly(termWrap);
    maybeShowRecordingNotice();
    createTerminal();
    term.writeln("\x1b[32m[logged in as " + currentUser + "]\x1b[0m");
    connect(ticket);
  }

  // 从终端返回会话列表：断 WS（长期会话仅 detach，保活）。
  function backToSessions() {
    closed = true;
    if (ws) {
      try { ws.close(); } catch (e) {}
      ws = null;
    }
    if (term) {
      try { term.dispose(); } catch (e) {}
      term = null;
    }
    currentSessionID = null;
    clearSession();
    showSessionPanel();
  }

  // ===== 会话面板 =====

  // 显示会话面板并刷新实时列表。
  // autoBashIfEmpty：列表为空且仅 bash 可用时，保持现状直接进入 bash 终端。
  function showSessionPanel(autoBashIfEmpty) {
    showOnly(sessionPanel);
    syncAuditButton();
    if (sessionUserEl) sessionUserEl.textContent = "已登录：" + (currentUser || "");
    // 按启用状态显隐新建模式按钮。
    newModeOpencode.classList.toggle("hidden", !cfg.opencodeEnabled);
    newModeCodex.classList.toggle("hidden", !cfg.codexEnabled);
    // 新建时长默认值。
    newHours.value = String(cfg.sessionTtlHours);
    newHours.max = String(cfg.maxSessionTtlHours);
    newTypeChanged();
    refreshSessions().then(function (list) {
      if (autoBashIfEmpty && list && list.length === 0 &&
          !cfg.opencodeEnabled && !cfg.codexEnabled) {
        quickBashIfNoSessions();
      }
    });
  }

  // 拉取并渲染会话列表；返回列表数组（失败返回 null）。
  function refreshSessions() {
    return apiJSON("/api/sessions")
      .then(function (r) {
        if (r.status === 401) {
          handleAuthExpired();
          return null;
        }
        if (r.ok && Array.isArray(r.data)) {
          renderSessions(r.data);
          return r.data;
        }
        showModal((r.data && r.data.error) || "加载会话列表失败");
        return null;
      })
      .catch(function () {
        showModal("网络错误，无法加载会话列表");
        return null;
      });
  }

  function formatRemaining(expireAt) {
    if (!expireAt) return "—";
    var ms = expireAt * 1000 - Date.now();
    if (ms <= 0) return "已到期";
    var totalMin = Math.floor(ms / 60000);
    var h = Math.floor(totalMin / 60);
    var m = totalMin % 60;
    if (h > 0) return h + "小时" + m + "分";
    return m + "分";
  }

  function formatCreatedAt(createdAt) {
    if (!createdAt) return "—";
    try {
      return new Date(createdAt * 1000).toLocaleString();
    } catch (e) {
      return "—";
    }
  }

  // 渲染会话列表（全部用 createElement/textContent，禁止 innerHTML 拼用户串）。
  function renderSessions(list) {
    sessionList.innerHTML = ""; // 清空容器，随后全部由代码创建

    var longCount = 0;
    for (var i = 0; i < list.length; i++) {
      if (list[i] && list[i].long_lived) longCount++;
    }
    var totalCount = list.length;

    if (sessionCountsEl) {
      sessionCountsEl.textContent =
        "长期 " + longCount + "/" + cfg.maxLongSessions +
        "　总会话 " + totalCount + "/" + cfg.maxTotalSessions;
    }

    if (list.length === 0) {
      sessionEmpty.classList.remove("hidden");
    } else {
      sessionEmpty.classList.add("hidden");
    }

    for (var j = 0; j < list.length; j++) {
      sessionList.appendChild(buildSessionRow(list[j]));
    }

    // 新建上限：长期名额与总名额。
    updateNewAvailability(longCount, totalCount);
  }

  function buildSessionRow(info) {
    var row = document.createElement("div");
    row.className = "session-row";

    var modeEl = document.createElement("span");
    modeEl.className = "session-mode";
    modeEl.textContent = info.mode || "bash";
    row.appendChild(modeEl);

    var typeEl = document.createElement("span");
    typeEl.className = "session-type" + (info.long_lived ? " long" : "");
    typeEl.textContent = info.long_lived ? "长期" : "临时";
    row.appendChild(typeEl);

    var statusEl = document.createElement("span");
    statusEl.className = "session-meta";
    statusEl.textContent = info.active ? "使用中" : "空闲";
    row.appendChild(statusEl);

    var createdEl = document.createElement("span");
    createdEl.className = "session-meta";
    createdEl.textContent = "创建：" + formatCreatedAt(info.created_at);
    row.appendChild(createdEl);

    var remainEl = document.createElement("span");
    remainEl.className = "session-meta";
    remainEl.textContent = info.long_lived
      ? "剩余：" + formatRemaining(info.expire_at)
      : "临时·空闲回收";
    row.appendChild(remainEl);

    var actions = document.createElement("div");
    actions.className = "session-actions";

    var enterBtn = document.createElement("button");
    enterBtn.type = "button";
    enterBtn.textContent = "进入";
    enterBtn.addEventListener("click", function () {
      attachSession(info.id, info.mode);
    });
    actions.appendChild(enterBtn);

    if (info.long_lived) {
      var extendWrap = document.createElement("span");
      extendWrap.className = "session-extend";

      var hoursInput = document.createElement("input");
      hoursInput.type = "number";
      hoursInput.min = "1";
      hoursInput.max = String(cfg.maxSessionTtlHours);
      hoursInput.value = "1";
      extendWrap.appendChild(hoursInput);

      var extendLabel = document.createElement("span");
      extendLabel.textContent = "小时";
      extendWrap.appendChild(extendLabel);

      var extendBtn = document.createElement("button");
      extendBtn.type = "button";
      extendBtn.textContent = "延期";
      extendBtn.addEventListener("click", function () {
        var hours = parseInt(hoursInput.value, 10);
        if (isNaN(hours) || hours < 1 || hours > cfg.maxSessionTtlHours) {
          showModal("延期时长需在 1 - " + cfg.maxSessionTtlHours + " 小时之间");
          return;
        }
        extendSession(info.id, hours);
      });
      extendWrap.appendChild(extendBtn);
      actions.appendChild(extendWrap);
    }

    var closeBtn = document.createElement("button");
    closeBtn.type = "button";
    closeBtn.className = "danger";
    closeBtn.textContent = "关闭";
    closeBtn.addEventListener("click", function () {
      closeSession(info.id);
    });
    actions.appendChild(closeBtn);

    row.appendChild(actions);
    return row;
  }

  // 根据当前计数更新新建区的可用状态与提示。
  function updateNewAvailability(longCount, totalCount) {
    var longFull = longCount >= cfg.maxLongSessions;
    var totalFull = totalCount >= cfg.maxTotalSessions;

    newTypeLong.disabled = longFull;
    if (longFull && newTypeLong.checked) {
      newTypeShort.checked = true;
    }
    newTypeChanged();

    var msg = "";
    if (totalFull) {
      msg = "会话总数已达上限（" + cfg.maxTotalSessions + "），请先关闭部分会话。";
    } else if (longFull) {
      msg = "长期会话数量已达上限（" + cfg.maxLongSessions + "），可新建临时会话。";
    }
    newError.textContent = msg;

    newModeBash.disabled = totalFull;
    newModeOpencode.disabled = totalFull;
    newModeCodex.disabled = totalFull;
  }

  // 新建类型切换：仅长期显示时长输入。
  function newTypeChanged() {
    var isLong = newTypeLong.checked && !newTypeLong.disabled;
    newHoursWrap.classList.toggle("hidden", !isLong);
  }

  // ===== 会话操作（ticket / extend / close / logout）=====

  // 用 mgmt 换一次性 ticket，成功后回调。
  function requestTicket(body, cb) {
    apiJSON("/api/session/ticket", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
      .then(function (r) {
        if (r.status === 401) {
          handleAuthExpired();
          cb(null);
          return;
        }
        if (r.ok && r.data && r.data.ticket) {
          cb(r.data.ticket);
        } else {
          showModal((r.data && r.data.error) || ("获取访问凭证失败（HTTP " + r.status + "）"));
          cb(null);
        }
      })
      .catch(function () {
        showModal("网络错误，无法获取访问凭证");
        cb(null);
      });
  }

  // 接入已有会话。
  function attachSession(sid, mode) {
    if (entering) return;
    entering = true;
    requestTicket({ action: "attach", session: sid }, function (ticket) {
      entering = false;
      if (ticket) enterTerminal(ticket, mode || "bash", sid);
    });
  }

  // 新建会话：type 为 "long" 或 ""（临时）。
  function createSession(mode) {
    if (entering) return;
    entering = true;
    var isLong = newTypeLong.checked && !newTypeLong.disabled;
    var body = { action: "create", mode: mode, type: isLong ? "long" : "" };
    if (isLong) {
      var hours = parseInt(newHours.value, 10);
      if (isNaN(hours) || hours < 1 || hours > cfg.maxSessionTtlHours) {
        newError.textContent =
          "时长需在 1 - " + cfg.maxSessionTtlHours + " 小时之间";
        entering = false;
        return;
      }
      body.hours = hours;
    }
    newError.textContent = "";
    requestTicket(body, function (ticket) {
      entering = false;
      if (ticket) enterTerminal(ticket, mode, null);
    });
  }

  // 延期（仅长期）。
  function extendSession(sid, hours) {
    apiJSON(
      "/api/session/extend?session=" + encodeURIComponent(sid) +
      "&hours=" + encodeURIComponent(hours),
      { method: "POST" }
    )
      .then(function (r) {
        if (r.status === 401) {
          handleAuthExpired();
          return;
        }
        if (r.ok) {
          refreshSessions();
        } else {
          showModal((r.data && r.data.error) || "延期失败");
        }
      })
      .catch(function () {
        showModal("网络错误，延期失败");
      });
  }

  // 关闭会话。
  function closeSession(sid) {
    apiJSON("/api/session/close?session=" + encodeURIComponent(sid), { method: "POST" })
      .then(function (r) {
        if (r.status === 401) {
          handleAuthExpired();
          return;
        }
        if (r.ok) {
          refreshSessions();
        } else {
          showModal((r.data && r.data.error) || "关闭会话失败");
        }
      })
      .catch(function () {
        showModal("网络错误，关闭会话失败");
      });
  }

  // 登出：吊销 mgmt_token，清空本地状态，回登录页。
  function logout() {
    var done = function () {
      clearMgmt();
      clearSession();
      showLogin("已登出");
    };
    apiFetch("/api/logout", { method: "POST" })
      .then(done)
      .catch(done);
  }

  // ===== 审计 / 回放 =====

  // 按登录配置显隐审计入口：审计员，或（启用录制且允许普通用户查看本人录屏）时展示。
  function syncAuditButton() {
    if (!btnAudit) return;
    var show = !!(cfg.isAuditor || (cfg.auditEnabled && cfg.userCanViewSelf));
    btnAudit.classList.toggle("hidden", !show);
  }

  // 录制明示：仅在启用录制时提示；已读过后不再重复。
  function maybeShowRecordingNotice() {
    if (!recordingNotice) return;
    if (!cfg.auditEnabled) return;
    var dismissed = false;
    try { dismissed = sessionStorage.getItem(AUDIT_NOTICE_KEY) === "1"; } catch (e) {}
    if (dismissed) return;
    recordingNotice.classList.remove("hidden");
  }

  function dismissRecordingNotice() {
    if (recordingNotice) recordingNotice.classList.add("hidden");
    try { sessionStorage.setItem(AUDIT_NOTICE_KEY, "1"); } catch (e) {}
  }

  // 打开审计列表页。
  function showAudit() {
    showOnly(auditPanel);
    if (auditUserFilter) auditUserFilter.classList.toggle("hidden", !cfg.isAuditor);
    maybeShowRecordingNotice();
    loadAuditList();
  }

  // 组装审计列表查询参数（纯函数，无副作用）。
  function buildQueryParams() {
    var p = [];
    if (cfg.isAuditor && auditUsernameFilter) {
      var u = auditUsernameFilter.value.trim();
      if (u) p.push("username=" + encodeURIComponent(u));
    }
    if (auditFromDate) p.push("from_date=" + encodeURIComponent(auditFromDate));
    if (auditToDate) p.push("to_date=" + encodeURIComponent(auditToDate));
    p.push("offset=" + String((auditPage - 1) * auditPageSize));
    p.push("limit=" + String(auditPageSize));
    return p;
  }

  // 更新分页控件状态与页码信息。
  function updatePagination(total) {
    if (!auditPrevBtn || !auditNextBtn || !auditPageInfo) return;
    var pageSize = auditPageSize;
    var totalPages = pageSize > 0 ? Math.ceil(total / pageSize) : 0;
    auditPageInfo.textContent = "第 " + auditPage + " / " + totalPages + " 页 · 共 " + total + " 条";
    auditPrevBtn.disabled = (auditPage <= 1);
    auditNextBtn.disabled = (auditPage >= totalPages || totalPages === 0);
  }

  // 读取上次审计列表偏好并回填控件（损坏则回退默认 pageSize=10、空日期）。
  function applyAuditPrefs() {
    var p = { pageSize: 10, from: "", to: "" };
    try {
      var raw = localStorage.getItem(AUDIT_PREFS_KEY);
      if (raw) {
        var o = JSON.parse(raw);
        if (o && typeof o === "object") {
          if (AUDIT_PAGE_SIZES.indexOf(o.pageSize) !== -1) p.pageSize = o.pageSize;
          if (typeof o.from === "string" && /^\d{4}-\d{2}-\d{2}$/.test(o.from)) p.from = o.from;
          if (typeof o.to === "string" && /^\d{4}-\d{2}-\d{2}$/.test(o.to)) p.to = o.to;
        }
      }
    } catch (e) {}
    auditPageSize = p.pageSize;
    auditFromDate = p.from;
    auditToDate = p.to;
    if (auditPageSizeSel) auditPageSizeSel.value = String(p.pageSize);
    if (auditDateFrom) auditDateFrom.value = p.from;
    if (auditDateTo) auditDateTo.value = p.to;
  }

  // 保存审计列表偏好（pageSize / 日期间隔）。
  function saveAuditPrefs() {
    try {
      localStorage.setItem(
        AUDIT_PREFS_KEY,
        JSON.stringify({ pageSize: auditPageSize, from: auditFromDate, to: auditToDate })
      );
    } catch (e) {}
  }

  // 拉取审计会话列表并渲染。普通用户不传 username（后端强制为本人）；
  // 审计员可传筛选值（留空=全部）。支持日期间隔、分页。
  function loadAuditList() {
    var params = buildQueryParams();
    var qs = params.length ? ("?" + params.join("&")) : "";
    apiJSON("/api/audit/sessions" + qs)
      .then(function (r) {
        if (r.status === 401) {
          handleAuthExpired();
          return;
        }
        if (r.ok) {
          var items = [];
          var total = 0;
          if (r.data && Array.isArray(r.data.items)) {
            items = r.data.items;
            total = typeof r.data.total === "number" ? r.data.total : items.length;
          } else if (Array.isArray(r.data)) {
            items = r.data;
            total = items.length;
          }
          auditTotal = total;
          // 越界自愈：当前页已超过总页数时回退第一页（保证只回退一次）。
          if (items.length === 0 && total > 0 && auditPage > 1) {
            auditPage = 1;
            loadAuditList();
            return;
          }
          renderAuditList(items);
          updatePagination(total);
        } else {
          showModal((r.data && r.data.error) || "加载录屏列表失败");
        }
      })
      .catch(function () {
        showModal("网络错误，无法加载录屏列表");
      });
  }

  // 时长格式化：3723 → "1h 2m 3s"。
  function formatDuration(secs) {
    if (secs === null || secs === undefined || isNaN(Number(secs))) return "—";
    var s = Math.floor(Number(secs));
    if (s <= 0) return "0s";
    var h = Math.floor(s / 3600);
    var m = Math.floor((s % 3600) / 60);
    var sec = s % 60;
    var parts = [];
    if (h) parts.push(h + "h");
    if (m) parts.push(m + "m");
    if (sec || parts.length === 0) parts.push(sec + "s");
    return parts.join(" ");
  }

  // 字节大小人类可读：1536 → "1.5 KB"。
  function formatBytes(n) {
    var b = Number(n);
    if (!b || b < 0 || isNaN(b)) return "0 B";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var i = 0;
    while (b >= 1024 && i < units.length - 1) {
      b /= 1024;
      i++;
    }
    var val = i === 0 ? String(b) : b.toFixed(b >= 100 ? 0 : 1);
    return val + " " + units[i];
  }

  // 相对秒格式化 [mm:ss]（输入面板用）。
  function formatClock(secs) {
    var s = Math.floor(Number(secs) || 0);
    if (s < 0) s = 0;
    var m = Math.floor(s / 60);
    var sec = s % 60;
    return (m < 10 ? "0" : "") + m + ":" + (sec < 10 ? "0" : "") + sec;
  }

  // 渲染审计列表（全部 createElement/textContent，禁止 innerHTML 拼用户串）。
  function renderAuditList(items) {
    if (!auditList) return;
    auditList.innerHTML = "";
    if (!items || items.length === 0) {
      if (auditEmpty) auditEmpty.classList.remove("hidden");
      return;
    }
    if (auditEmpty) auditEmpty.classList.add("hidden");
    for (var i = 0; i < items.length; i++) {
      auditList.appendChild(buildAuditRow(items[i] || {}));
    }
  }

  // 构建单条录屏行。
  function buildAuditRow(info) {
    var row = document.createElement("div");
    row.className = "audit-row";

    if (cfg.isAuditor) {
      var userEl = document.createElement("span");
      userEl.className = "audit-user";
      userEl.textContent = info.username || "—";
      row.appendChild(userEl);
    }

    var modeEl = document.createElement("span");
    modeEl.className = "audit-mode";
    modeEl.textContent = info.mode || "bash";
    row.appendChild(modeEl);

    if (info.active) {
      var badge = document.createElement("span");
      badge.className = "audit-active";
      badge.textContent = "进行中";
      row.appendChild(badge);
    }

    var createdEl = document.createElement("span");
    createdEl.className = "audit-meta";
    createdEl.textContent = "创建：" + formatCreatedAt(info.created_at);
    row.appendChild(createdEl);

    var durEl = document.createElement("span");
    durEl.className = "audit-meta";
    durEl.textContent = "时长：" + formatDuration(info.duration_secs);
    row.appendChild(durEl);

    var ipEl = document.createElement("span");
    ipEl.className = "audit-meta";
    ipEl.textContent = "来源：" + (info.ip || "—");
    row.appendChild(ipEl);

    var sizeEl = document.createElement("span");
    sizeEl.className = "audit-meta";
    sizeEl.textContent = "大小：" + formatBytes(info.size);
    row.appendChild(sizeEl);

    var actions = document.createElement("div");
    actions.className = "audit-actions";

    if (!info.active) {
      var playBtn = document.createElement("button");
      playBtn.type = "button";
      playBtn.textContent = "回放";
      playBtn.addEventListener("click", function () {
        openPlayback(info);
      });
      actions.appendChild(playBtn);
    }

    if (cfg.isAuditor && !info.active) {
      var dlBtn = document.createElement("button");
      dlBtn.type = "button";
      dlBtn.textContent = "下载";
      dlBtn.addEventListener("click", function () {
        downloadAudit(info.session_id);
      });
      actions.appendChild(dlBtn);
    }

    row.appendChild(actions);
    return row;
  }

  // 动态注入 asciinema-player.js（遵守 CSP script-src 'self'），加载完成后回调。
  // 已加载则立即回调；加载中则排队，避免重复注入。
  function ensurePlayerLoaded(cb) {
    if (playerScriptLoaded && typeof AsciinemaPlayer !== "undefined") {
      cb();
      return;
    }
    playerLoadCbs.push(cb);
    if (playerScriptLoading) return;
    playerScriptLoading = true;
    var s = document.createElement("script");
    s.src = "/static/vendor/asciinema-player.js";
    s.onload = function () {
      playerScriptLoaded = true;
      playerScriptLoading = false;
      var cbs = playerLoadCbs;
      playerLoadCbs = [];
      for (var i = 0; i < cbs.length; i++) {
        try { cbs[i](); } catch (e) {}
      }
    };
    s.onerror = function () {
      playerScriptLoading = false;
      playerLoadCbs = [];
      showModal("回放组件加载失败");
    };
    document.head.appendChild(s);
  }

  // 销毁上一个播放器实例（v3 实例带 dispose()，会清空容器内容）。
  function disposePlayer() {
    if (playerInstance) {
      try {
        if (typeof playerInstance.dispose === "function") playerInstance.dispose();
      } catch (e) {}
      playerInstance = null;
    }
    if (playbackEl) playbackEl.innerHTML = "";
  }

  // 打开某条录屏的回放页。
  function openPlayback(item) {
    if (!item || item.active) return;
    currentPlaybackItem = item;
    showOnly(playbackPanel);
    if (playbackTitle) {
      playbackTitle.textContent =
        "回放：" + (item.username || "") + " / " + (item.mode || "bash");
    }
    // 仅审计员可见「显示输入」开关；默认关闭、清空输入面板。
    if (showInputWrap) showInputWrap.classList.toggle("hidden", !cfg.isAuditor);
    if (showInputToggle) showInputToggle.checked = false;
    if (inputPanel) {
      inputPanel.innerHTML = "";
      inputPanel.classList.add("hidden");
    }
    ensurePlayerLoaded(renderPlayer);
  }

  // 取 cast 文本并以 {data} 内联模式交给 asciinema-player 播放。
  // 不使用 Blob URL / {url}：CSP connect-src 'self' 无 blob:，且同源 url 直载
  // 带不上 Authorization 头，故内联最稳。
  function renderPlayer() {
    if (!currentPlaybackItem) return;
    var sid = currentPlaybackItem.session_id;
    apiFetch("/api/audit/cast?session=" + encodeURIComponent(sid))
      .then(function (resp) {
        if (resp.status === 401) {
          handleAuthExpired();
          return null;
        }
        if (!resp.ok) {
          showModal("加载录制失败（HTTP " + resp.status + "）");
          return null;
        }
        return resp.text();
      })
      .then(function (text) {
        if (text === null || text === undefined) return;
        disposePlayer();
        try {
          playerInstance = AsciinemaPlayer.create(
            { data: text },
            playbackEl,
            { autoPlay: false, speed: 1, controls: true }
          );
        } catch (e) {
          showModal("回放器初始化失败");
        }
        buildInputPanel(text);
      })
      .catch(function () {
        showModal("网络错误，无法加载录制");
      });
  }

  // 解析 asciicast NDJSON，收集 i（input）事件并渲染到输入面板（默认隐藏）。
  function buildInputPanel(castText) {
    if (!inputPanel) return;
    inputPanel.innerHTML = "";
    if (!castText) return;
    var lines = castText.split("\n");
    var entries = [];
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i].trim();
      if (!line) continue;
      var ev;
      try {
        ev = JSON.parse(line);
      } catch (e) {
        continue;
      }
      if (!Array.isArray(ev) || ev.length < 3) continue; // 首行 header 为对象，跳过
      if (ev[1] !== "i") continue;
      entries.push({ t: ev[0], data: String(ev[2]) });
    }
    if (entries.length === 0) {
      var empty = document.createElement("div");
      empty.className = "input-empty";
      empty.textContent = "（无输入事件，或输入已按策略省略/脱敏）";
      inputPanel.appendChild(empty);
      return;
    }
    for (var j = 0; j < entries.length; j++) {
      var row = document.createElement("div");
      row.className = "input-row";

      var timeEl = document.createElement("span");
      timeEl.className = "input-time";
      timeEl.textContent = "[" + formatClock(entries[j].t) + "]";
      row.appendChild(timeEl);

      var dataEl = document.createElement("span");
      dataEl.className = "input-data";
      dataEl.textContent = entries[j].data;
      row.appendChild(dataEl);

      inputPanel.appendChild(row);
    }
  }

  // 下载录屏（审计员专属）：arraybuffer → Blob → a[download] 触发下载。
  function downloadAudit(sid) {
    if (!sid) return;
    apiFetch("/api/audit/download?session=" + encodeURIComponent(sid))
      .then(function (resp) {
        if (resp.status === 401) {
          handleAuthExpired();
          return null;
        }
        if (!resp.ok) {
          showModal("下载失败（HTTP " + resp.status + "）");
          return null;
        }
        return resp.arrayBuffer();
      })
      .then(function (buf) {
        if (!buf) return;
        var url = URL.createObjectURL(new Blob([buf]));
        var a = document.createElement("a");
        a.href = url;
        a.download = sid + ".zip";
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        setTimeout(function () {
          try { URL.revokeObjectURL(url); } catch (e) {}
        }, 1000);
      })
      .catch(function () {
        showModal("网络错误，下载失败");
      });
  }

  // ===== 刷新恢复 =====

  // 页面刷新：sessionStorage 有 mgmt_token → 免重登。
  // 若还存有当前会话 sid，先探测 /api/resume，ok 则直接重连，否则回会话列表。
  function tryResumeSession(saved) {
    apiFetch("/api/resume?session=" + encodeURIComponent(saved.sid))
      .then(function (resp) {
        return resp.json().then(
          function (data) { return { ok: resp.ok, status: resp.status, data: data }; },
          function () { return { ok: resp.ok, status: resp.status, data: null }; }
        );
      })
      .then(function (r) {
        if (r.status === 401) {
          handleAuthExpired();
          return;
        }
        if (r.ok && r.data && isTrue(r.data.ok)) {
          attachSession(saved.sid, r.data.mode || saved.mode);
        } else {
          clearSession();
          showSessionPanel(true);
        }
      })
      .catch(function () {
        // 网络异常无法探测：回到会话列表（由列表操作重试）。
        showSessionPanel(true);
      });
  }

  // ===== 事件绑定 =====

  // 登录表单提交。
  loginForm.addEventListener("submit", function (e) {
    e.preventDefault();
    var username = usernameInput.value.trim();
    var password = passwordInput.value;
    loginError.textContent = "";

    fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: username, password: password }),
    })
      .then(function (resp) {
        return resp.json().then(function (data) {
          return { ok: resp.ok, status: resp.status, data: data };
        });
      })
      .then(function (result) {
        if (result.ok && result.data && result.data.mgmt_token) {
          var d = result.data;
          mgmtToken = d.mgmt_token;
          currentUser = d.username;
          applyConfig(d);
          saveMgmt(mgmtToken, currentUser, d);
          clearSession();
          loginError.textContent = "";
          showSessionPanel(true);
        } else {
          var msg = (result.data && result.data.error) ||
            ("登录失败（HTTP " + result.status + "）");
          loginError.textContent = msg;
        }
      })
      .catch(function () {
        loginError.textContent = "网络错误，无法连接服务端";
      });
  });

  // 会话面板：登出。
  btnLogout.addEventListener("click", logout);

  // 会话面板：新建类型切换。
  newTypeLong.addEventListener("change", newTypeChanged);
  newTypeShort.addEventListener("change", newTypeChanged);

  // 会话面板：新建（各模式）。
  newModeBash.addEventListener("click", function () { createSession("bash"); });
  newModeOpencode.addEventListener("click", function () { createSession("opencode"); });
  newModeCodex.addEventListener("click", function () { createSession("codex"); });

  // 终端工具条：返回会话列表。
  btnSessions.addEventListener("click", backToSessions);

  // 字号控制：减小 / 增大。
  btnFontMinus.addEventListener("click", function () {
    applyFontSize(currentFontSize - FONT_STEP);
  });
  btnFontPlus.addEventListener("click", function () {
    applyFontSize(currentFontSize + FONT_STEP);
  });

  // 主题切换。
  themeSelect.addEventListener("change", function () {
    applyTheme(themeSelect.value);
  });

  // 文件上传：点击按钮前先校验会话有效，否则弹窗提示且不打开文件选择框。
  btnUpload.addEventListener("click", function () {
    if (!currentSessionID) {
      showModal("请先进入终端后，再进行文件上传");
      return;
    }
    fileInput.click();
  });
  fileInput.addEventListener("change", function () {
    var f = fileInput.files && fileInput.files[0];
    if (f) uploadFile(f);
    fileInput.value = "";
  });

  // 消息弹窗「确定」关闭。
  modalOk.addEventListener("click", hideModal);

  // API Key 弹窗「确定」/「取消」。
  authOkBtn.addEventListener("click", authOk);
  authCancelBtn.addEventListener("click", authCancel);

  // 上传弹窗「取消上传」：中止当前上传。
  uploadCancel.addEventListener("click", cancelUpload);

  // 审计入口。
  btnAudit.addEventListener("click", showAudit);

  // 审计列表：用户筛选（审计员）。结果集变化时回到第 1 页。
  auditFilterBtn.addEventListener("click", function () {
    auditPage = 1;
    loadAuditList();
  });
  auditUsernameFilter.addEventListener("keydown", function (e) {
    if (e.key === "Enter") {
      e.preventDefault();
      auditPage = 1;
      loadAuditList();
    }
  });

  // 审计列表：日期间隔筛选。结果集变化时回到第 1 页。
  auditDateFilterBtn.addEventListener("click", function () {
    auditFromDate = auditDateFrom.value;
    auditToDate = auditDateTo.value;
    auditPage = 1;
    saveAuditPrefs();
    loadAuditList();
  });
  auditDateResetBtn.addEventListener("click", function () {
    if (auditDateFrom) auditDateFrom.value = "";
    if (auditDateTo) auditDateTo.value = "";
    auditFromDate = "";
    auditToDate = "";
    auditPage = 1;
    saveAuditPrefs();
    loadAuditList();
  });
  // 日期输入框回车即触发筛选。
  auditDateFrom.addEventListener("keydown", function (e) {
    if (e.key === "Enter") {
      e.preventDefault();
      auditDateFilterBtn.click();
    }
  });
  auditDateTo.addEventListener("keydown", function (e) {
    if (e.key === "Enter") {
      e.preventDefault();
      auditDateFilterBtn.click();
    }
  });

  // 审计列表：单页行数切换。回到第 1 页并记住偏好。
  auditPageSizeSel.addEventListener("change", function () {
    auditPageSize = parseInt(auditPageSizeSel.value, 10) || 10;
    auditPage = 1;
    saveAuditPrefs();
    loadAuditList();
  });

  // 审计列表：分页上一页 / 下一页。
  auditPrevBtn.addEventListener("click", function () {
    if (auditPrevBtn.disabled) return;
    auditPage = auditPage > 1 ? auditPage - 1 : 1;
    loadAuditList();
  });
  auditNextBtn.addEventListener("click", function () {
    if (auditNextBtn.disabled) return;
    auditPage = auditPage + 1;
    loadAuditList();
  });

  // 审计列表 → 返回会话面板。
  btnAuditBack.addEventListener("click", function () {
    showSessionPanel();
  });

  // 回放页 → 返回审计列表（释放播放器并重载列表）。
  btnPlaybackBack.addEventListener("click", function () {
    disposePlayer();
    currentPlaybackItem = null;
    showOnly(auditPanel);
    loadAuditList();
  });

  // 输入面板显隐（仅审计员可见该开关）。
  showInputToggle.addEventListener("change", function () {
    if (!inputPanel) return;
    inputPanel.classList.toggle("hidden", !showInputToggle.checked);
  });

  // 录制明示「知道了」。
  recordingNoticeOk.addEventListener("click", dismissRecordingNotice);

  // ===== 初始化 =====

  // 登录入口：无会话且仅 bash 时，直接新建一个临时 bash 会话进入终端。
  function quickBashIfNoSessions() {
    if (entering) return;
    entering = true;
    // 当前产品行为：无会话时保持「直接进入 bash」的轻量路径。
    requestTicket({ action: "create", mode: "bash", type: "" }, function (ticket) {
      entering = false;
      if (ticket) enterTerminal(ticket, "bash", null);
    });
  }

  function init() {
    applyAuditPrefs();
    var m = loadMgmt();
    if (!m) {
      showLogin();
      return;
    }
    mgmtToken = m.token;
    currentUser = m.username;
    applyConfig(m);
    var s = loadSession();
    if (s && s.sid) {
      tryResumeSession(s);
    } else {
      showSessionPanel(true);
    }
  }

  init();
})();
