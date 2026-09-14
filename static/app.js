// Web Shell 前端逻辑
// 负责：登录（POST /api/login 获取一次性 token）、WebSocket 连接、
//       xterm 事件绑定、消息编解码。

(function () {
  "use strict";

  // 检查 xterm 是否加载成功（CDN）。
  if (typeof Terminal === "undefined") {
    document.body.innerHTML =
      "<p style='color:#f88;font-family:monospace;padding:20px;'>" +
      "Failed to load xterm.js. Please check your network / CDN access.</p>";
    return;
  }

  var loginPanel = document.getElementById("login-panel");
  var loginForm = document.getElementById("login-form");
  var usernameInput = document.getElementById("username");
  var passwordInput = document.getElementById("password");
  var loginError = document.getElementById("login-error");
  var terminalEl = document.getElementById("terminal");

  var termWrap = document.getElementById("term-wrap");
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

  var modePanel = document.getElementById("mode-panel");
  var modeUserEl = document.getElementById("mode-user");
  var btnBash = document.getElementById("btn-bash");
  var btnOpencode = document.getElementById("btn-opencode");
  var welcomeEl = document.getElementById("toolbar-welcome");

  var term = null;
  var fitAddon = null;
  var ws = null;
  var closed = false;

  // 当前会话标识（用于页面刷新后恢复同一终端），以及暂存 token/用户名。
  var currentSessionID = null;
  var currentToken = null;
  var currentUser = null;
  var currentMode = "bash";
  var SESSION_STORAGE_KEY = "webshell.session";

  // 会话持久化：把 sessionID/username/mode 存入 sessionStorage，
  // 页面刷新后据此自动恢复同一终端（当前标签页内有效，关闭标签即清除）。
  function saveSession(sid, username, mode) {
    if (!sid || !username) return;
    try {
      sessionStorage.setItem(
        SESSION_STORAGE_KEY,
        JSON.stringify({ sid: sid, username: username, mode: mode || "bash" })
      );
    } catch (e) {}
  }

  function loadSession() {
    try {
      var raw = sessionStorage.getItem(SESSION_STORAGE_KEY);
      if (!raw) return null;
      var o = JSON.parse(raw);
      if (o && o.sid && o.username) return o;
    } catch (e) {}
    return null;
  }

  function clearSession() {
    currentSessionID = null;
    try { sessionStorage.removeItem(SESSION_STORAGE_KEY); } catch (e) {}
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

  // 建立 WebSocket 连接。
  //  - 新会话：携带一次性 token 与 mode（"bash" 或 "opencode"）。
  //  - 恢复会话：携带 session + username，服务端据此重新附着到同一终端。
  function connect(token, mode) {
    closed = false;
    var url =
      "ws://" + location.host + "/ws?token=" + encodeURIComponent(token) +
      "&mode=" + encodeURIComponent(mode || "bash");
    if (currentSessionID) {
      url += "&session=" + encodeURIComponent(currentSessionID) +
             "&username=" + encodeURIComponent(currentUser || "");
    }
    ws = new WebSocket(url);

    ws.onopen = function () {
      term.writeln("\x1b[32m[connected]\x1b[0m");
      // 连接建立后发送一次初始尺寸。
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
          saveSession(sid, currentUser, currentMode);
        }
      } else if (msg.type === "close") {
        var reason = (msg.data && msg.data.reason) || "unknown";
        term.writeln("\r\n\x1b[31m[closed: " + reason + "]\x1b[0m");
        term.dispose();
        closed = true;
        if (ws) {
          ws.close();
          ws = null;
        }
        // 回到登录界面。
        showLogin("会话已结束：" + reason);
      }
    };

    ws.onerror = function () {
      term.writeln("\r\n\x1b[31m[websocket error]\x1b[0m");
    };

    ws.onclose = function () {
      if (!closed) {
        term.writeln("\r\n\x1b[31m[connection lost]\x1b[0m");
      }
    };
  }

  // 创建终端并挂载。
  function createTerminal() {
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
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "input", data: data }));
      }
    });

    // 终端尺寸变化 -> 发送 resize。
    term.onResize(function () {
      sendResize();
    });

    // 窗口尺寸变化 -> 重新 fit 并发送 resize。
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

  // 显示消息弹窗（div，替代浏览器 alert）。
  function showModal(message) {
    modalMessage.textContent = message || "";
    modalOverlay.style.display = "flex";
    modalOk.focus();
  }

  // 隐藏消息弹窗。
  function hideModal() {
    modalOverlay.style.display = "none";
  }

  // 显示上传进度弹窗并重置进度条。
  function showUploadProgress(filename) {
    uploadFilename.textContent = filename || "";
    uploadBar.style.width = "0%";
    uploadPercent.textContent = "0%";
    uploadTitle.textContent = "正在上传...";
    uploadOverlay.style.display = "flex";
  }

  // 更新上传进度（0-100）。
  function setUploadProgress(pct) {
    if (pct < 0) pct = 0;
    if (pct > 100) pct = 100;
    uploadBar.style.width = pct + "%";
    uploadPercent.textContent = pct + "%";
  }

  // 隐藏上传进度弹窗。
  function hideUploadProgress() {
    uploadOverlay.style.display = "none";
  }

  // 上传前先做权限预检（GET /api/upload-check），目标目录不可写时直接弹窗提示，
  // 不启动上传，避免上传完成后写文件才报权限错误。
  function uploadFile(file) {
    if (!currentSessionID || !currentUser) {
      showModal("上传失败：无有效会话");
      return;
    }

    var checkUrl =
      "/api/upload-check?session=" + encodeURIComponent(currentSessionID) +
      "&username=" + encodeURIComponent(currentUser);
    fetch(checkUrl)
      .then(function (resp) {
        return resp.json().then(function (data) {
          return { ok: resp.ok, data: data };
        });
      })
      .then(function (r) {
        if (r.ok && r.data && r.data.ok === "true") {
          // 权限校验通过，开始实际上传。
          doUpload(file);
        } else {
          var msg = (r.data && r.data.error) || "当前目录不可写";
          showModal(msg);
        }
      })
      .catch(function () {
        // 预检请求失败（网络等）：直接弹窗提示，不进入上传。
        showModal("无法检查目录权限，请稍后重试");
      });
  }

  // 实际执行上传（假设已通过权限预检）。目标目录由服务端取会话实时工作目录决定；
  // 失败（尤其是权限不足）时以 div 弹窗提示，不在终端中显示。
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
      "/api/upload?session=" + encodeURIComponent(currentSessionID) +
      "&username=" + encodeURIComponent(currentUser),
      true
    );

    // 上传进度回调。
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
      if (xhr.status === 200 && data && data.ok === "true") {
        term.writeln("\x1b[32m[上传成功：" + (data.path || file.name) + "]\x1b[0m");
        sendEnter(); // 自动回车，回到 shell 交互提示符
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

  // 取消当前进行中的上传：终止请求并关闭进度弹窗。
  function cancelUpload() {
    if (activeUploadXhr) {
      activeUploadXhr.abort();
      activeUploadXhr = null;
      hideUploadProgress();
      term.writeln("\r\n\x1b[33m[上传已取消]\x1b[0m");
      sendEnter(); // 自动回车，回到 shell 交互提示符
    }
  }

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
    termWrap.style.display = "none";
    modePanel.style.display = "none";
    loginPanel.style.display = "flex";
    loginError.textContent = message || "";
    usernameInput.value = "";
    passwordInput.value = "";
    usernameInput.focus();
  }

  // 设置工具栏居中的欢迎词。
  function setWelcome(username) {
    if (welcomeEl) {
      welcomeEl.textContent = "欢迎使用 Web Shell，";
      if (username) welcomeEl.textContent = "欢迎您，" + username + "！";
    }
  }

  // 登录成功后进入终端（mode 为 "bash" 或 "opencode"）。新会话：sessionID 待
  // 服务端下发后才会保存，因此这里先清空旧的会话记录。
  function enterTerminal(token, username, mode) {
    currentToken = token;
    currentUser = username;
    currentMode = mode || "bash";
    clearSession(); // 重置，等待服务端下发新 sessionID
    setWelcome(username);
    loginPanel.style.display = "none";
    modePanel.style.display = "none";
    termWrap.style.display = "flex";
    createTerminal();
    term.writeln("\x1b[32m[logged in as " + username + "]\x1b[0m");
    connect(token, mode);
  }

  // 恢复会话：页面刷新后自动重连到同一终端（无需重新登录）。
  function enterTerminalResume(username, mode) {
    currentToken = ""; // 恢复会话无需一次性 token
    currentUser = username;
    currentMode = mode || "bash";
    setWelcome(username);
    loginPanel.style.display = "none";
    modePanel.style.display = "none";
    termWrap.style.display = "flex";
    createTerminal();
    connect("", mode);
  }

  // 页面加载时尝试恢复会话：先在 /api/resume 探测会话是否仍存活，
  // 存活则直接进入终端，否则清除记录并显示登录框。
  function initResume() {
    var s = loadSession();
    if (!s) {
      showLogin();
      return;
    }
    var url = "/api/resume?session=" + encodeURIComponent(s.sid) +
              "&username=" + encodeURIComponent(s.username);
    fetch(url)
      .then(function (resp) {
        if (resp.ok) {
          currentSessionID = s.sid;
          enterTerminalResume(s.username, s.mode);
        } else {
          clearSession();
          showLogin("会话已失效，请重新登录");
        }
      })
      .catch(function () {
        // 网络异常无法探测：尝试直接重连，失败再由 onclose 回登录框。
        currentSessionID = s.sid;
        enterTerminalResume(s.username, s.mode);
      });
  }

  // 显示模式选择面板（仅当 opencode 启用时调用）。
  function showModeSelection(token, username) {
    loginPanel.style.display = "none";
    termWrap.style.display = "none";
    modePanel.style.display = "flex";
    modeUserEl.textContent = "已登录：" + username + "，请选择会话模式";
  }

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
        if (result.ok && result.data && result.data.token) {
          currentToken = result.data.token;
          currentUser = result.data.username;
          // opencode 已启用时展示选择面板，否则直接进入 bash。
          if (result.data.opencode_enabled) {
            showModeSelection(result.data.token, result.data.username);
          } else {
            enterTerminal(result.data.token, result.data.username, "bash");
          }
        } else {
          // 显示服务端返回的错误文案。
          var msg = (result.data && result.data.error) || "登录失败（HTTP " + result.status + "）";
          loginError.textContent = msg;
        }
      })
      .catch(function () {
        loginError.textContent = "网络错误，无法连接服务端";
      });
  });

  // 模式选择：打开 Bash 终端。
  btnBash.addEventListener("click", function () {
    enterTerminal(currentToken, currentUser, "bash");
  });

  // 模式选择：打开 OpenCode。
  btnOpencode.addEventListener("click", function () {
    enterTerminal(currentToken, currentUser, "opencode");
  });

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
    if (!currentSessionID || !currentUser) {
      showModal("请先登录并进入终端后，再进行文件上传");
      return;
    }
    fileInput.click();
  });
  // 选择文件后立即上传。
  fileInput.addEventListener("change", function () {
    var f = fileInput.files && fileInput.files[0];
    if (f) uploadFile(f);
    fileInput.value = ""; // 允许再次选择同一文件
  });

  // 消息弹窗「确定」关闭。
  modalOk.addEventListener("click", hideModal);

  // 上传弹窗「取消上传」：中止当前上传。
  uploadCancel.addEventListener("click", cancelUpload);

  // 初始：尝试恢复上次会话，否则显示登录面板。
  initResume();
})();
