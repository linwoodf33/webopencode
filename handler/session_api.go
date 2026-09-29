package handler

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os/user"
	"strconv"

	"webshell/auth"
)

// ctxKey 是本包使用的 context key 类型。
type ctxKey int

const ctxUserKey ctxKey = iota

// lookupHome 解析用户家目录（供 create ticket 预解析）。测试可替换。
var lookupHome = func(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// 名额上限提示文案（与测试矩阵一致）。
const (
	msgLongLimit  = "长期会话数量已达上限"
	msgTotalLimit = "会话总数已达上限"
)

// ctxUsername 从请求 context 取 requireMgmt 注入的用户名；无则返回空串。
func ctxUsername(r *http.Request) string {
	v, _ := r.Context().Value(ctxUserKey).(string)
	return v
}

// bearerToken 从 Authorization 头解析 Bearer token；无则返回空串。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && equalFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

// equalFold 判断字符串是否大小写不敏感地等于 ASCII 目标（仅用于前缀比较）。
func equalFold(s, target string) bool {
	if len(s) != len(target) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		t := target[i]
		if 'A' <= t && t <= 'Z' {
			t += 'a' - 'A'
		}
		if c != t {
			return false
		}
	}
	return true
}

// RequireMgmt 返回一个中间件：要求 Bearer mgmt_token，校验通过后将用户名注入
// 请求 context（键 ctxUserKey）。校验失败返回 401。
func RequireMgmt(mgmt *auth.MgmtTokenStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing mgmt token"})
				return
			}
			username, ok := mgmt.Validate(raw)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid or expired mgmt token"})
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUserKey, username)))
		})
	}
}

// sessionInfoDTO 是 /api/sessions 返回的单条会话信息（时间为 Unix 秒）。
type sessionInfoDTO struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	LongLived bool   `json:"long_lived"`
	CreatedAt int64  `json:"created_at"`
	ExpireAt  int64  `json:"expire_at"`
	Active    bool   `json:"active"`
}

// NewSessionsHandler 返回 GET /api/sessions 处理函数：返回该用户实时会话列表
// （按 createdAt 倒序）。仅索引读取，不触碰本地会话。
func NewSessionsHandler(mgr *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		infos := mgr.index.List(ctxUsername(r))
		out := make([]sessionInfoDTO, 0, len(infos))
		for _, info := range infos {
			out = append(out, sessionInfoDTO{
				ID:        info.ID,
				Mode:      info.Mode,
				LongLived: info.LongLived,
				CreatedAt: info.CreatedAt.Unix(),
				ExpireAt:  info.ExpireAt.Unix(),
				Active:    info.Attached,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// ticketRequest 是 POST /api/session/ticket 请求体。
type ticketRequest struct {
	Action  string  `json:"action"`
	Session string  `json:"session"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	Hours   float64 `json:"hours"`
}

// ticketResponse 是 ticket 响应体。
type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

// validMode 判断会话模式是否合法（空串视为 bash）。
func validMode(mode string) string {
	switch mode {
	case "":
		return "bash"
	case "bash", "opencode", "codex":
		return mode
	default:
		return ""
	}
}

// NewTicketHandler 返回 POST /api/session/ticket 处理函数。
//
// 在此完成全部校验（归属、名额、hours 范围、mode），换发一次性 ticket。
func NewTicketHandler(mgr *SessionManager, tickets *auth.TicketStore, cfg *auth.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		var req ticketRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
			return
		}
		username := ctxUsername(r)

		switch req.Action {
		case "attach":
			sid := req.Session
			info, ok := mgr.index.Get(sid)
			// 不存在 / 归属不符一律 404（防枚举）。
			if !ok || info.Username != username {
				writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
				return
			}
			if info.expired(mgr.clock()) {
				writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
				return
			}
			tok, err := tickets.Issue(auth.TicketPurpose{
				Action:   "attach",
				Username: username,
				Session:  sid,
				Mode:     info.Mode,
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to issue ticket"})
				return
			}
			writeJSON(w, http.StatusOK, ticketResponse{Ticket: tok, ExpiresIn: ticketTTLSeconds})
			return

		case "create":
			mode := validMode(req.Mode)
			if mode == "" {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid mode"})
				return
			}
			typ := req.Type
			if typ == "" {
				typ = "short"
			}
			longLived := typ == "long"
			if !longLived && typ != "short" {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid type"})
				return
			}

			hours := req.Hours
			if longLived {
				if hours == 0 {
					hours = cfg.SessionTTLHours
				}
				// hours 必须为整数（拒 2.5 等被 int() 截断的输入）且在范围内。
				if hours < 1 || hours > cfg.MaxSessionTTLHours || hours != math.Floor(hours) {
					writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid hours"})
					return
				}
			} else {
				hours = 0
			}

			// 名额预检（真正约束由索引 Add 的二次校验保证）。
			if mgr.index.TotalCount(username) >= cfg.MaxTotalN {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: msgTotalLimit})
				return
			}
			if longLived && mgr.index.LongCount(username) >= cfg.MaxLongN {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: msgLongLimit})
				return
			}

			homeDir, err := lookupHome(username)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "user not provisioned on this host"})
				return
			}

			tok, err := tickets.Issue(auth.TicketPurpose{
				Action:   "create",
				Username: username,
				HomeDir:  homeDir,
				Mode:     mode,
				Type:     typ,
				Hours:    int(hours),
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to issue ticket"})
				return
			}
			writeJSON(w, http.StatusOK, ticketResponse{Ticket: tok, ExpiresIn: ticketTTLSeconds})
			return

		default:
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid action"})
		}
	}
}

// NewExtendHandler 返回 POST /api/session/extend?session=<id>&hours=<n> 处理函数。
// 仅长期、未过期会话可延期；expireAt += hours。
func NewExtendHandler(mgr *SessionManager, cfg *auth.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		hours, err := strconv.Atoi(r.URL.Query().Get("hours"))
		if err != nil || hours < 1 || float64(hours) > cfg.MaxSessionTTLHours {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid hours"})
			return
		}
		info, err := mgr.index.Extend(r.URL.Query().Get("session"), ctxUsername(r), hours)
		switch err {
		case nil:
			writeJSON(w, http.StatusOK, map[string]any{"id": info.ID, "expire_at": info.ExpireAt.Unix()})
		case ErrSessionNotFound:
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found"})
		default:
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		}
	}
}

// NewCloseHandler 返回 POST /api/session/close?session=<id> 处理函数。
// 校验归属后关闭会话（统一走 manager.Close）。
func NewCloseHandler(mgr *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		sid := r.URL.Query().Get("session")
		info, ok := mgr.index.Get(sid)
		if !ok || info.Username != ctxUsername(r) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found"})
			return
		}
		mgr.Close(sid)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// NewLogoutHandler 返回 POST /api/logout 处理函数：吊销当前 mgmt_token。
func NewLogoutHandler(mgmt *auth.MgmtTokenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		mgmt.Revoke(bearerToken(r))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// NewResumeHandler 返回 GET /api/resume?session=<sid> 处理函数。
// 仅探测会话是否可恢复，不消费 ticket。需经 requireMgmt 包装。
func NewResumeHandler(mgr *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		sid := r.URL.Query().Get("session")
		info, ok := mgr.index.Get(sid)
		if !ok || info.Username != ctxUsername(r) || info.expired(mgr.clock()) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
			return
		}
		ls := mgr.GetLocal(sid)
		if ls == nil || ls.isClosed() {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": info.Mode, "long_lived": info.LongLived})
	}
}

// ticketTTLSeconds ticket 有效期（秒），与 auth 包固定 60s 对齐，用于响应 expires_in。
const ticketTTLSeconds = 60
