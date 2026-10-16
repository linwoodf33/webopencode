package handler

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"webshell/audit"
	"webshell/auth"
)

// AuditAPI 承载审计 REST 端点的共享依赖（store 为 nil 表示审计未启用）。
//
// 权限模型见 DESIGN_AUDIT.md §7：所有端点先经 requireMgmt（注入 username），
// 越权一律 404（防枚举）。
type AuditAPI struct {
	mgr   *SessionManager
	store *audit.Store
	cfg   *auth.Config
}

// NewAuditAPI 构造审计 API 聚合对象。
func NewAuditAPI(mgr *SessionManager, store *audit.Store, cfg *auth.Config) *AuditAPI {
	return &AuditAPI{mgr: mgr, store: store, cfg: cfg}
}

// RequireAuditor 返回中间件：校验 requireMgmt 已注入的 username 属于审计员白名单。
// 非审计员一律 404（防枚举，与既有会话接口一致）。须置于 RequireMgmt 内层。
func RequireAuditor(cfg *auth.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg == nil || !cfg.IsAuditor(ctxUsername(r)) {
				writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// notFound 统一 404 响应（防枚举）。
func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
}

// selfVisible 判断普通用户是否被允许查看本人录屏（user_can_view_self 关闭时仅审计员可看）。
func (api *AuditAPI) selfVisible(auditor bool) bool {
	if auditor {
		return true
	}
	return api.cfg != nil && api.cfg.AuditUserSelf
}

// scanOwner 审计员路径：分页扫描 store.List 定位 sid 的归属用户名与 meta。
// 起因：audit.Store 未提供「按 sessionID 反查 username」的公开方法，而前端仅传
// session 参数，故必须借 List 扫描（DESIGN_AUDIT §9）。
func (api *AuditAPI) scanOwner(sid string) (string, audit.SessionMeta, bool) {
	const page = 2000
	offset := 0
	for {
		items, total, err := api.store.List(audit.ListOptions{Offset: offset, Limit: page})
		if err != nil {
			return "", audit.SessionMeta{}, false
		}
		for _, it := range items {
			if it.SessionID != sid {
				continue
			}
			m, ok, err := api.store.ReadMeta(it.Username, sid)
			if err != nil || !ok {
				return "", audit.SessionMeta{}, false
			}
			return it.Username, m, true
		}
		if len(items) == 0 {
			return "", audit.SessionMeta{}, false
		}
		offset += len(items)
		if offset >= total {
			return "", audit.SessionMeta{}, false
		}
	}
}

// resolveMeta 解析会话 meta 并做归属校验。
//
// 返回 (meta, okAndAllowed, notFound)：
//   - store 未启用 / sid 为空 / 不存在 / 越权 → when ok=false（handler 一律回 404）；
//   - notFound 区分「不存在」与「越权」，语义上均为 404（防枚举），仅供调试/测试。
//
// 越权判定：non-auditor 强制 meta.Username == ctxUsername；且 user_can_view_self
// 关闭时 non-auditor 即便本人也拒绝（§7.3）。
func (api *AuditAPI) resolveMeta(r *http.Request, sid string) (*audit.SessionMeta, bool, bool) {
	if api.store == nil || sid == "" {
		return nil, false, true
	}
	user := ctxUsername(r)
	auditor := api.cfg != nil && api.cfg.IsAuditor(user)
	if !api.selfVisible(auditor) {
		return nil, false, true
	}

	var owner string
	var meta audit.SessionMeta
	var found bool
	if auditor {
		owner, meta, found = api.scanOwner(sid)
	} else {
		m, ok, err := api.store.ReadMeta(user, sid)
		if err == nil && ok {
			owner, meta, found = user, m, true
		}
	}
	if !found {
		return nil, false, true
	}
	if !auditor && owner != user {
		return nil, false, true
	}
	// 以目录归属为准回填，避免损坏 meta 的 username 为空导致下游路径错位。
	meta.Username = owner
	return &meta, true, false
}

// NewAuditSessionsHandler 返回 GET /api/audit/sessions 处理函数。
//
// 普通用户忽略传入 username、强制为本人；审计员可留空（全部）或指定。支持
// from_date/to_date（"2006-01-02"）与 offset/limit 分页。响应 {"total","items"}。
func NewAuditSessionsHandler(api *AuditAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		if api.store == nil {
			notFound(w)
			return
		}
		user := ctxUsername(r)
		auditor := api.cfg != nil && api.cfg.IsAuditor(user)
		if !api.selfVisible(auditor) {
			notFound(w)
			return
		}

		q := r.URL.Query()
		username := q.Get("username")
		if !auditor {
			username = user // 普通用户强制本人
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))

		items, total, err := api.store.List(audit.ListOptions{
			Username: username,
			FromDate: q.Get("from_date"),
			ToDate:   q.Get("to_date"),
			Offset:   offset,
			Limit:    limit,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to list audit sessions"})
			return
		}
		if items == nil {
			items = []audit.SessionItem{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": total, "items": items})
	}
}

// NewAuditMetaHandler 返回 GET /api/audit/meta?session=<sid> 处理函数。
// 返回 meta.json 原对象（含时间线）。
func NewAuditMetaHandler(api *AuditAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		meta, ok, _ := api.resolveMeta(r, r.URL.Query().Get("session"))
		if !ok {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, meta)
	}
}

// NewAuditCastHandler 返回 GET /api/audit/cast?session=<sid>&t_from=&t_to= 处理函数。
//
// 无 t_from/t_to 时原样返回 cast.log 字节（application/x-asciicast）；有则按相对秒
// 窗口切片并平移 -t_from（audit.SliceCast）。cast.log 不存在 → 404。
func NewAuditCastHandler(api *AuditAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		sid := r.URL.Query().Get("session")
		meta, ok, _ := api.resolveMeta(r, sid)
		if !ok {
			notFound(w)
			return
		}
		if api.store.IsActive(sid) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: "active session"})
			return
		}
		raw, err := api.store.ReadCast(meta.Username, sid)
		if err != nil {
			notFound(w)
			return
		}

		w.Header().Set("Content-Type", "application/x-asciicast; charset=utf-8")

		q := r.URL.Query()
		tFromStr, tToStr := q.Get("t_from"), q.Get("t_to")
		if tFromStr == "" && tToStr == "" {
			_, _ = w.Write(raw)
			return
		}
		tFrom, err1 := strconv.ParseFloat(tFromStr, 64)
		tTo, err2 := strconv.ParseFloat(tToStr, 64)
		if err1 != nil || err2 != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid time window"})
			return
		}
		if err := audit.SliceCast(bytes.NewReader(raw), tFrom, tTo, w); err != nil {
			log.Printf("audit: slice cast %s: %v", sid, err)
		}
	}
}

// NewAuditDownloadHandler 返回 GET /api/audit/download?session=<sid> 处理函数（仅审计员，
// 由 RequireAuditor 保证）。打包 meta.json + cast.log 为 zip。
func NewAuditDownloadHandler(api *AuditAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		sid := r.URL.Query().Get("session")
		meta, ok, _ := api.resolveMeta(r, sid)
		if !ok {
			notFound(w)
			return
		}
		if api.store.IsActive(sid) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: "active session"})
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sid+"-audit.zip"))
		if err := api.store.Download(meta.Username, sid, w); err != nil {
			// 响应头可能已写出，无法再改状态码；记录日志即可。
			log.Printf("audit: download %s: %v", sid, err)
		}
	}
}
