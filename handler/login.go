package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"webshell/auth"
)

// loginRequest 登录请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse 登录成功响应体。
type loginResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	// OpencodeEnabled 是否启用 opencode 模式，供前端决定是否展示「打开 opencode」选项。
	OpencodeEnabled bool `json:"opencode_enabled"`
}

// errorResponse 错误响应体。
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// loginStatus 返回认证错误对应的 HTTP 状态码与文案。
// 未知错误统一按 500 处理。
func loginStatus(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrInvalidFormat):
		return http.StatusBadRequest, "invalid username format"
	case errors.Is(err, auth.ErrInvalidEmail):
		return http.StatusBadRequest, "invalid email format"
	case errors.Is(err, auth.ErrUserNotFound):
		return http.StatusUnauthorized, "user not found or not authorized"
	case errors.Is(err, auth.ErrRootNotAllowed):
		return http.StatusForbidden, "root is not allowed"
	case errors.Is(err, auth.ErrNotProvisioned):
		return http.StatusForbidden, "user not provisioned on this host"
	case errors.Is(err, auth.ErrServiceUnavailable):
		// 服务端故障（AD 不可达/绑定失败）。502/503 均可，用 503 表示服务暂不可用。
		return http.StatusServiceUnavailable, "authentication service unavailable"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// loginLogName 返回可安全写入日志的标识名：纯账号名可直接记录，
// 邮箱为敏感信息且含 @，绝不打印原文（统一记为 <email> 占位）。
func loginLogName(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '@' {
			return "<email>"
		}
	}
	return raw
}

// Login 处理 POST /api/login。
// opencodeEnabled 表示 opencode 模式是否启用，随登录响应返回给前端。
func Login(a *auth.Authenticator, opencodeEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}

		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid username format"})
			return
		}

		// 明确提示「密码缺失」：requirePassword 开启且未提供密码时直接 400，
		// 与用户名格式错误区分文案，便于前端提示（绝不记录密码内容）。
		if a.RequirePassword() && req.Username != "" && req.Password == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "password required"})
			return
		}

		// 日志只记录安全标识名（邮箱绝不打印原文）与错误 sentinel，绝不记录密码。
		token, username, _, err := a.Login(req.Username, req.Password)
		if err != nil {
			status, msg := loginStatus(err)
			log.Printf("login failed for account %q: %v", loginLogName(req.Username), err)
			writeJSON(w, status, errorResponse{Error: msg})
			return
		}

		log.Printf("login %q ok", username)
		writeJSON(w, http.StatusOK, loginResponse{Token: token, Username: username, OpencodeEnabled: opencodeEnabled})
	}
}
