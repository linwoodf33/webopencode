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

  var term = null;
  var fitAddon = null;
  var ws = null;
  var closed = false;

  // 当前会话标识（用于页面刷新后恢复同一终端），以及管理凭证/用户名。
  var currentSessionID = null;
  var mgmtToken = null;
  var currentUser = null;
  var currentMode = "bash";

  // 服务端下发的会话配置（登录响应提供），用于面板默认值与上限校验。
  var cfg = {
    opencodeEnabled: false,
    codexEnabled: false,
    sessionTtlHours: 24,
    maxSessionTtlHours: 168,
    maxLongSessions: 3,
    maxTotalSessions: 10,
  };

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
    var panels = [loginPanel, modePanel, sessionPanel, termWrap];
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

  // 通过 WebSocket 向终端发送一次回车（\r），用于上传成功/取消后让终端
  // 回到 shell 交互提示符界面。
  function sendEnter() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: "input", data: "\r" }));
  }

  // 建立 WebSocket 连接：仅携带一次性 ticket（60s）。
  // 新建与接入均先经 /api/session/ticket 换取 ticket，再走本函数。
  function connect(ticket) {
    closed = false;
    var proto = location.protocol === "https:" ? "wss://" : "ws://";
    var url = proto + location.host + "/ws?ticket=" + encodeURIComponent(ticket);
    ws = new WebSocket(url);

    ws.onopen = function () {
      term.writeln("\x1b[32m[connected]\x1b[0m");
      sendResize();
    };

    ws.onmessage = function (evt) {
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

    ws.onerror = function () {
      if (term) term.writeln("\r\n\x1b[31m[websocket error]\x1b[0m");
    };

    ws.onclose = function () {
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
    });

    fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(terminalEl);
    fitAddon.fit();
    fontSizeLabel.textContent = currentFontSize + "px";
    themeSelect.value = currentTheme;

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

    window.addEventListener("resize", function () {
      if (fitAddon) fitAddon.fit();
      sendResize();
    });
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
    if (sid) {
      saveSession(sid, currentMode);
    } else {
      clearSession();
    }
    setWelcome(currentUser);
    showOnly(termWrap);
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
          return;
        }
        if (r.ok && r.data && r.data.ticket) {
          cb(r.data.ticket);
        } else {
          showModal((r.data && r.data.error) || ("获取访问凭证失败（HTTP " + r.status + "）"));
        }
      })
      .catch(function () {
        showModal("网络错误，无法获取访问凭证");
      });
  }

  // 接入已有会话。
  function attachSession(sid, mode) {
    requestTicket({ action: "attach", session: sid }, function (ticket) {
      enterTerminal(ticket, mode || "bash", sid);
    });
  }

  // 新建会话：type 为 "long" 或 ""（临时）。
  function createSession(mode) {
    var isLong = newTypeLong.checked && !newTypeLong.disabled;
    var body = { action: "create", mode: mode, type: isLong ? "long" : "" };
    if (isLong) {
      var hours = parseInt(newHours.value, 10);
      if (isNaN(hours) || hours < 1 || hours > cfg.maxSessionTtlHours) {
        newError.textContent =
          "时长需在 1 - " + cfg.maxSessionTtlHours + " 小时之间";
        return;
      }
      body.hours = hours;
    }
    newError.textContent = "";
    requestTicket(body, function (ticket) {
      enterTerminal(ticket, mode, null);
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

  // ===== 初始化 =====

  // 登录入口：无会话且仅 bash 时，直接新建一个临时 bash 会话进入终端。
  function quickBashIfNoSessions() {
    // 当前产品行为：无会话时保持「直接进入 bash」的轻量路径。
    requestTicket({ action: "create", mode: "bash", type: "" }, function (ticket) {
      enterTerminal(ticket, "bash", null);
    });
  }

  function init() {
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
