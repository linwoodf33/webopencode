package audit

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store 是审计存储根：管理 <root>/<username>/<sessionID>/{meta.json,cast.log} 布局，
// 提供列表/读取/切片/删除/清扫与启动巡检。
type Store struct {
	root   string
	opt    Options
	active func() map[string]bool
	now    func() time.Time
}

// NewStore 创建审计存储：确保 root 存在且可写（自检写一个探针文件后删除）。
func NewStore(root string, opt Options) (*Store, error) {
	if root == "" {
		return nil, errors.New("audit: empty store root")
	}
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, err
	}
	// 可写自检。
	probe := filepath.Join(root, ".writecheck")
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, castFileMode)
	if err != nil {
		return nil, fmt.Errorf("audit: store root not writable: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return &Store{root: root, opt: opt, now: time.Now}, nil
}

// Open 在 root/<username>/<sessionID> 下打开一个会话录制器（先做路径安全校验）。
func (s *Store) Open(meta SessionMeta) (*Recorder, error) {
	dir, ok := s.sessionDir(meta.Username, meta.SessionID)
	if !ok {
		return nil, fmt.Errorf("audit: invalid username/session id (%q/%q)", meta.Username, meta.SessionID)
	}
	return Open(dir, meta, s.opt)
}

// SetActiveResolver 注入活跃会话解析器（返回当前未 Close 的 sessionID 集合）；清扫据此排除。
func (s *Store) SetActiveResolver(f func() map[string]bool) {
	s.active = f
}

// ListOptions 控制 List 的过滤与分页。
type ListOptions struct {
	Username string // 空=全部
	FromDate string // "YYYY-MM-DD"（UTC 日，含）
	ToDate   string // "YYYY-MM-DD"（UTC 日，不含）
	Offset   int
	Limit    int // <=0→100；>2000 截 2000
}

// SessionItem 是列表返回的单条会话摘要。
type SessionItem struct {
	SessionID        string `json:"session_id"`
	Username         string `json:"username"`
	Mode             string `json:"mode"`
	LongLived        bool   `json:"long_lived"`
	CreatedAt        int64  `json:"created_at"`
	EndedAt          int64  `json:"ended_at,omitempty"`
	EndReason        string `json:"end_reason,omitempty"`
	DurationSecs     int64  `json:"duration_secs"`
	IP               string `json:"ip,omitempty"`
	Size             int64  `json:"size"`
	MaskedEvents     int64  `json:"masked_input_events,omitempty"`
	DroppedEvents    int64  `json:"dropped_events,omitempty"`
	StoppedRecording bool   `json:"stopped_recording,omitempty"`
	Active           bool   `json:"active"`
}

// List 返回会话摘要（按 CreatedAt 倒序）与其总数（过滤后、分页前）。
func (s *Store) List(o ListOptions) ([]SessionItem, int, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 2000 {
		limit = 2000
	}

	users, err := s.listUsers(o.Username)
	if err != nil {
		return nil, 0, err
	}

	var items []SessionItem
	for _, u := range users {
		ents, err := os.ReadDir(filepath.Join(s.root, u))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, 0, err
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			sid := e.Name()
			if !validIDComponent(sid) {
				continue
			}
			meta, ok, err := s.ReadMeta(u, sid)
			if err != nil || !ok {
				continue
			}
			if !inDateRange(meta.CreatedAt, o.FromDate, o.ToDate) {
				continue
			}
			items = append(items, s.toItem(u, sid, meta))
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].SessionID > items[j].SessionID
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})

	total := len(items)
	start := o.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return items[start:end], total, nil
}

// ReadMeta 读取指定会话的 meta.json。不存在返回 (零值,false,nil)。
func (s *Store) ReadMeta(username, sessionID string) (SessionMeta, bool, error) {
	p, ok := s.metaPath(username, sessionID)
	if !ok {
		return SessionMeta{}, false, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionMeta{}, false, nil
		}
		return SessionMeta{}, false, err
	}
	var m SessionMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return SessionMeta{}, false, err
	}
	return m, true, nil
}

// ReadCast 读取指定会话的 cast.log 原文。不存在返回 os.ErrNotExist。
func (s *Store) ReadCast(username, sessionID string) ([]byte, error) {
	p, ok := s.castPath(username, sessionID)
	if !ok {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(p)
}

// Download 将会话的 meta.json + cast.log 打包为 zip 写入 w（cast 命名为 <sid>.cast）。
//
// offlineHTML 非空时，在 meta.json、<sid>.cast 之后追加第三个条目 offline-player.html
//（自包含离线回放页，内容 = offlineHTML，Deflate）。offlineHTML 为空则仅打包两文件，
// 行为与未引入离线页时完全一致（模板缺失/数据异常时的降级路径）。
func (s *Store) Download(username, sessionID string, w io.Writer, offlineHTML []byte) error {
	dir, ok := s.sessionDir(username, sessionID)
	if !ok {
		return os.ErrNotExist
	}
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	if err := addFileToZip(zw, filepath.Join(dir, "meta.json"), "meta.json"); err != nil {
		_ = zw.Close()
		return err
	}
	if err := addFileToZip(zw, filepath.Join(dir, "cast.log"), sessionID+".cast"); err != nil {
		_ = zw.Close()
		return err
	}
	if len(offlineHTML) > 0 {
		if err := addBytesToZip(zw, offlineHTML, "offline-player.html"); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

// CleanOnce 执行一次清扫：保留期（按 ended_at）与总量配额（最旧优先），均排除活跃会话。
// 返回删除的会话目录数。
func (s *Store) CleanOnce(now time.Time) (int, error) {
	if s.opt.RetentionDays <= 0 && s.opt.MaxTotalBytes <= 0 {
		return 0, nil
	}

	type cand struct {
		dir  string
		meta SessionMeta
		size int64
	}
	var all []cand

	users, err := s.listUsers("")
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		ents, err := os.ReadDir(filepath.Join(s.root, u))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			sid := e.Name()
			if !validIDComponent(sid) || s.isActive(sid) {
				continue
			}
			meta, ok, err := s.ReadMeta(u, sid)
			if err != nil || !ok {
				continue
			}
			dir := filepath.Join(s.root, u, sid)
			var size int64
			if fi, serr := os.Stat(filepath.Join(dir, "cast.log")); serr == nil {
				size = fi.Size()
			}
			all = append(all, cand{dir: dir, meta: meta, size: size})
		}
	}

	deleted := 0
	// 保留期：ended_at 早于 now-retention 的删除。
	var remain []cand
	if s.opt.RetentionDays > 0 {
		limit := time.Duration(s.opt.RetentionDays) * 24 * time.Hour
		for _, c := range all {
			if c.meta.EndedAt != nil && now.Sub(*c.meta.EndedAt) > limit {
				if err := os.RemoveAll(c.dir); err != nil {
					return deleted, err
				}
				deleted++
				continue
			}
			remain = append(remain, c)
		}
	} else {
		remain = all
	}

	// 总量配额：累计超限则按 ended_at 最旧优先（无 ended_at 视为最旧）删除。
	if s.opt.MaxTotalBytes > 0 {
		var total int64
		for _, c := range remain {
			total += c.size
		}
		if total > s.opt.MaxTotalBytes {
			sort.SliceStable(remain, func(i, j int) bool {
				return endedLess(remain[i].meta.EndedAt, remain[j].meta.EndedAt)
			})
			for _, c := range remain {
				if total <= s.opt.MaxTotalBytes {
					break
				}
				if err := os.RemoveAll(c.dir); err != nil {
					return deleted, err
				}
				total -= c.size
				deleted++
			}
		}
	}
	return deleted, nil
}

// StartCleanup 启动后台清扫：先跑一次，随后按 interval 周期执行，ctx.Done 退出。
// interval<=0 表示不启动。
func (s *Store) StartCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		if _, err := s.CleanOnce(s.clock()); err != nil {
			log.Printf("audit: cleanup: %v", err)
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := s.CleanOnce(s.clock()); err != nil {
					log.Printf("audit: cleanup: %v", err)
				}
			}
		}
	}()
}

// ScanAndFinalize 启动巡检：为缺少 ended_at 的会话补写结束态（幂等，已有结束态跳过）。
//
// ended_at = cast.log 最后一个事件时间 + started_at（无事件则用 cast.log mtime）；
// end_reason = "server restart (abnormal)"；timeline 追加 close 事件；meta 原子写。
func (s *Store) ScanAndFinalize() error {
	users, err := s.listUsers("")
	if err != nil {
		return err
	}
	for _, u := range users {
		ents, err := os.ReadDir(filepath.Join(s.root, u))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			sid := e.Name()
			if !validIDComponent(sid) || s.isActive(sid) {
				continue
			}
			dir := filepath.Join(s.root, u, sid)

			var meta SessionMeta
			b, rerr := os.ReadFile(filepath.Join(dir, "meta.json"))
			switch {
			case rerr == nil:
				if jerr := json.Unmarshal(b, &meta); jerr != nil {
					// 损坏 meta：按「缺 ended_at」容错，用最小元数据补写。
					meta = SessionMeta{}
				}
			case errors.Is(rerr, os.ErrNotExist):
				continue
			default:
				return rerr
			}
			if meta.SessionID == "" {
				meta.SessionID = sid
			}
			if meta.Username == "" {
				meta.Username = u
			}
			if meta.EndedAt != nil {
				continue
			}
			if meta.StartedAt.IsZero() {
				if fi, serr := os.Stat(filepath.Join(dir, "cast.log")); serr == nil {
					meta.StartedAt = fi.ModTime()
				} else {
					meta.StartedAt = s.clock()
				}
			}

			ended := s.lastCastTime(dir, meta)
			meta.EndedAt = &ended
			if meta.EndReason == "" {
				meta.EndReason = "server restart (abnormal)"
			}
			meta.Timeline = append(meta.Timeline, TimelineEvent{
				T:     s.clock().Unix(),
				Event: "close",
				Extra: map[string]any{"reason": "server restart (abnormal)"},
			})
			if err := writeMetaFile(filepath.Join(dir, "meta.json"), meta); err != nil {
				return err
			}
		}
	}
	return nil
}

// toItem 由会话 meta 构造列表项。
func (s *Store) toItem(username, sid string, meta SessionMeta) SessionItem {
	it := SessionItem{
		SessionID:        meta.SessionID,
		Username:         username,
		Mode:             meta.Mode,
		LongLived:        meta.LongLived,
		CreatedAt:        meta.CreatedAt.Unix(),
		EndReason:        meta.EndReason,
		IP:               meta.Client.IP,
		MaskedEvents:     meta.MaskedEvents,
		DroppedEvents:    meta.Dropped,
		StoppedRecording: meta.Stopped,
		Active:           s.isActive(sid),
	}
	if it.SessionID == "" {
		it.SessionID = sid
	}
	if meta.EndedAt != nil {
		it.EndedAt = meta.EndedAt.Unix()
		it.DurationSecs = meta.EndedAt.Unix() - meta.CreatedAt.Unix()
	}
	if fi, err := os.Stat(filepath.Join(s.root, username, sid, "cast.log")); err == nil {
		it.Size = fi.Size()
	}
	return it
}

// lastCastTime 返回 cast.log 最后一条事件的时间（started_at + 最后一事件相对秒）；
// 无有效事件则回退到 cast.log mtime，再回退到 meta.StartedAt/now。
func (s *Store) lastCastTime(dir string, meta SessionMeta) time.Time {
	castPath := filepath.Join(dir, "cast.log")
	f, err := os.Open(castPath)
	if err != nil {
		if !meta.StartedAt.IsZero() {
			return meta.StartedAt
		}
		return s.clock()
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	first := true
	var lastT float64
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if first {
			first = false
			continue // 跳过 header
		}
		ev, perr := ParseEventLine(line)
		if perr != nil {
			continue
		}
		lastT = ev.T
		found = true
	}
	if found && !meta.StartedAt.IsZero() {
		return meta.StartedAt.Add(time.Duration(lastT * float64(time.Second)))
	}
	if fi, serr := os.Stat(castPath); serr == nil {
		return fi.ModTime()
	}
	if !meta.StartedAt.IsZero() {
		return meta.StartedAt
	}
	return s.clock()
}

// listUsers 返回候选用户名列表：username 非空时仅校验该用户；空时列举根下所有子目录。
func (s *Store) listUsers(username string) ([]string, error) {
	if username != "" {
		if !validUsername(username) {
			return nil, nil
		}
		if fi, err := os.Stat(filepath.Join(s.root, username)); err != nil || !fi.IsDir() {
			return nil, nil
		}
		return []string{username}, nil
	}
	ents, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var users []string
	for _, e := range ents {
		if e.IsDir() {
			users = append(users, e.Name())
		}
	}
	return users, nil
}

// sessionDir 返回会话目录并做路径安全校验。失败返回 ok=false。
func (s *Store) sessionDir(username, sessionID string) (string, bool) {
	if !validUsername(username) || !validIDComponent(sessionID) {
		return "", false
	}
	return filepath.Join(s.root, username, sessionID), true
}

// metaPath 返回 meta.json 路径。
func (s *Store) metaPath(username, sessionID string) (string, bool) {
	d, ok := s.sessionDir(username, sessionID)
	if !ok {
		return "", false
	}
	return filepath.Join(d, "meta.json"), true
}

// castPath 返回 cast.log 路径。
func (s *Store) castPath(username, sessionID string) (string, bool) {
	d, ok := s.sessionDir(username, sessionID)
	if !ok {
		return "", false
	}
	return filepath.Join(d, "cast.log"), true
}

// isActive 查询会话是否活跃（未 Close）。
func (s *Store) isActive(sid string) bool {
	if s.active == nil {
		return false
	}
	return s.active()[sid]
}

// IsActive 报告会话是否活跃（未 Close）。nil resolver 视为无活跃。
func (s *Store) IsActive(sid string) bool { return s.isActive(sid) }

// clock 返回当前时间（尊重注入时钟）。
func (s *Store) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// validIDComponent 校验可作为单层目录名的标识：非空、len<=200、每字符 ∈ [0-9A-Za-z_-]。
// 不含 "."、"/"、"\ "，从源头阻断路径穿越。
func validIDComponent(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// validUsername 校验用户名可作为安全的单层目录名：非空、len<=200、不等于 "."/".."、
// 不含路径分隔符与控制字符（允许 AD 常见的点号，如 john.doe）。
func validUsername(s string) bool {
	if s == "" || len(s) > 200 || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			return false
		}
	}
	return true
}

// inDateRange 判断 t 的 UTC 日是否落在 [from,to)：from 含、to 不含；空串表示不限。
func inDateRange(t time.Time, from, to string) bool {
	if from == "" && to == "" {
		return true
	}
	d := t.UTC().Format("2006-01-02")
	if from != "" && d < from {
		return false
	}
	if to != "" && d >= to {
		return false
	}
	return true
}

// endedLess 比较两个结束时间：nil 视为最旧（优先删除）。
func endedLess(a, b *time.Time) bool {
	if a == nil {
		return true
	}
	if b == nil {
		return false
	}
	return a.Before(*b)
}

// addFileToZip 将 src 以 name 加入 zip。
func addFileToZip(zw *zip.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	hdr.Name = name
	hdr.Method = zip.Deflate
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// addBytesToZip 将内存字节以 name 加入 zip（Deflate）。
func addBytesToZip(zw *zip.Writer, data []byte, name string) error {
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// writeMetaFile 原子写 meta.json：同目录临时文件 + fsync + rename。
func writeMetaFile(path string, meta SessionMeta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".meta-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	remove = false
	return nil
}
