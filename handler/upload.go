package handler

import (
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"webshell/pty"
)

// maxUploadBytes 单次上传文件大小上限（100MB）。
const maxUploadBytes = 100 << 20

// writeFileAsUser / checkWritableAsUser 是 pty 文件操作的测试接缝（seam）：
// 生产实现直接转发到 *pty.Session；单测可替换为打桩，避免依赖真实 pty/sudo。
var (
	writeFileAsUser = func(s *pty.Session, filename string, data []byte) (string, error) {
		return s.WriteFileAsUser(filename, data)
	}
	checkWritableAsUser = func(s *pty.Session) (string, error) {
		return s.CheckWritableAsUser("")
	}
)

// NewUploadHandler 处理 POST /api/upload（将文件上传到当前终端目录）。
//
// 经 requireMgmt 认证后，通过 ?session=<id> 定位会话（并校验归属当前 mgmt 用户），
// 取其实时工作目录（WorkingDirectory）作为上传目标，再以目标用户身份写入文件，
// 确保文件归属该用户。权限不足时返回明确提示，引导用户先在终端中切换到有写权限的目录。
func NewUploadHandler(mgr *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}

		username := ctxUsername(r)
		hs := mgr.GetLocal(r.URL.Query().Get("session"))
		if hs == nil || hs.username != username || hs.isClosed() {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
			return
		}

		// 解析 multipart，读取文件名与内容。
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid multipart form"})
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing file field"})
			return
		}
		defer file.Close()

		// 仅取 basename，杜绝路径穿越。
		filename := filepath.Base(header.Filename)
		if filename == "" || filename == "." || filename == string(filepath.Separator) {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid filename"})
			return
		}

		data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to read upload"})
			return
		}
		if len(data) > maxUploadBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "file too large (max 100MB)"})
			return
		}

		dest, err := writeFileAsUser(hs.proc, filename, data)
		if err != nil {
			log.Printf("upload failed: user=%s file=%s: %v", hs.username, filename, err)
			msg := "上传失败：" + err.Error()
			if isPermissionErr(err) {
				msg = "无法写入当前目录（权限不足），请在终端中切换到有写权限的目录后重试"
			}
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true", "path": dest})
	}
}

// isPermissionErr 判断错误是否为权限/目录类错误（据此提示用户切换目录）。
func isPermissionErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "not a directory")
}

// NewUploadCheckHandler 处理 GET /api/upload-check（上传前权限预检）。
//
// 经 requireMgmt 认证后，通过 ?session=<id> 定位会话（并校验归属），取其实时工作
// 目录，以目标用户身份检测该目录是否可写（临时文件写入+删除）。可写返回
// 200 {ok:true, path}；不可写返回 400 并附提示，前端据此在上传前就提醒用户切换目录。
func NewUploadCheckHandler(mgr *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}

		username := ctxUsername(r)
		hs := mgr.GetLocal(r.URL.Query().Get("session"))
		if hs == nil || hs.username != username || hs.isClosed() {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found or expired"})
			return
		}

		dir, err := checkWritableAsUser(hs.proc)
		if err != nil {
			log.Printf("upload-check failed: user=%s: %v", hs.username, err)
			msg := "无法写入当前目录（权限不足），请在终端中切换到有写权限的目录后重试"
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true", "path": dir})
	}
}
