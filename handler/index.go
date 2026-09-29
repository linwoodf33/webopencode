package handler

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// 索引默认参数。
const (
	// defaultShortTTL 短期会话空闲保活时长（硬编码，不进 yaml）。
	defaultShortTTL = 5 * time.Minute
	// defaultReaperInterval 索引 reaper 默认巡检间隔。
	defaultReaperInterval = 30 * time.Second
)

// 名额上限 sentinel error。
var (
	// ErrLongLimit 长期会话数量已达上限。
	ErrLongLimit = errors.New("long session limit reached")
	// ErrTotalLimit 会话总数已达上限。
	ErrTotalLimit = errors.New("total session limit reached")
	// ErrSessionNotFound 索引中不存在该会话（或归属不匹配）。
	ErrSessionNotFound = errors.New("session not found")
	// ErrNotLongLived 会话非长期，不可延期。
	ErrNotLongLived = errors.New("session is not long-lived")
	// ErrSessionExpired 会话已过期，不可延期。
	ErrSessionExpired = errors.New("session expired")
)

// SessionInfo 索引中的会话元数据。
//
// 元数据/索引/TTL/归属由索引维护，可替换为外置实现；Attached 表示索引视角下
// 该会话当前是否有连接（由 manager 在 attach/detach 时更新），reaper 据此判断
// 短期会话是否可回收。
type SessionInfo struct {
	ID        string
	Username  string
	Mode      string
	LongLived bool
	CreatedAt time.Time
	ExpireAt  time.Time
	Attached  bool
}

// expired 判断会话在 now 时刻是否已过期（与 reaper 语义一致）：
//   - 长期：now >= expireAt（无论是否 attached）；
//   - 短期：!attached && now >= expireAt。
func (info SessionInfo) expired(now time.Time) bool {
	if info.LongLived {
		return !now.Before(info.ExpireAt)
	}
	return !info.Attached && !now.Before(info.ExpireAt)
}

// memoryIndex 内存会话索引，维护元数据/TTL/归属，并驱动统一 reaper。
//
// 锁层级：indexMu → mgrMu → sMu。持有 indexMu 时绝不回调 manager/localSession/
// *pty.Session；reaper 仅在锁内取元数据快照，随后在锁外调 closeFn（即
// manager.Close）。
type memoryIndex struct {
	mu             sync.Mutex
	now            func() time.Time
	shortTTL       time.Duration
	reaperInterval time.Duration
	sessions       map[string]SessionInfo

	// closeFn 由 manager 注入：在索引锁外调用，用于终结到期会话。
	closeFn func(id string)
}

// NewIndex 创建一个内存索引。
//
//   - shortTTL <= 0 → 使用 defaultShortTTL（5min）；
//   - reaperInterval < 0 → 不启动 reaper；== 0 → 使用 defaultReaperInterval
//     (30s) 并启动；> 0 → 以该间隔启动；
//   - now == nil → time.Now。
func NewIndex(shortTTL, reaperInterval time.Duration, now func() time.Time) *memoryIndex {
	if shortTTL <= 0 {
		shortTTL = defaultShortTTL
	}
	if now == nil {
		now = time.Now
	}
	ix := &memoryIndex{
		now:      now,
		shortTTL: shortTTL,
		sessions: make(map[string]SessionInfo),
	}
	if reaperInterval >= 0 {
		if reaperInterval == 0 {
			reaperInterval = defaultReaperInterval
		}
		ix.reaperInterval = reaperInterval
		stop := make(chan struct{})
		go ix.runReaper(stop)
	}
	return ix
}

// SetNow 注入时钟（供测试使用）。f 为 nil 时忽略。
func (ix *memoryIndex) SetNow(f func() time.Time) {
	if f == nil {
		return
	}
	ix.mu.Lock()
	ix.now = f
	ix.mu.Unlock()
}

// clock 返回当前时间（尊重注入的时钟）。
func (ix *memoryIndex) clock() time.Time {
	ix.mu.Lock()
	f := ix.now
	ix.mu.Unlock()
	if f == nil {
		return time.Now()
	}
	return f()
}

// setCloseFunc 注入会话终结回调（由 manager 在构造时调用）。
func (ix *memoryIndex) setCloseFunc(fn func(id string)) {
	ix.mu.Lock()
	ix.closeFn = fn
	ix.mu.Unlock()
}

// Add 加入一个会话元数据，并在索引锁内原子校验名额（二次校验，消除预检竞态）：
//   - 长期会话：LongCount < maxLong 且 TotalCount < maxTotal；
//   - 短期会话：TotalCount < maxTotal。
//
// 超额返回 ErrLongLimit / ErrTotalLimit 且不写入。maxLong/maxTotal <= 0 表示不限制。
func (ix *memoryIndex) Add(info SessionInfo, maxLong, maxTotal int) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	if maxTotal > 0 && ix.totalCountLocked(info.Username) >= maxTotal {
		return ErrTotalLimit
	}
	if info.LongLived && maxLong > 0 && ix.longCountLocked(info.Username) >= maxLong {
		return ErrLongLimit
	}
	ix.sessions[info.ID] = info
	return nil
}

// Update 更新会话的 expireAt。不存在返回 false。
func (ix *memoryIndex) Update(id string, expireAt time.Time) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	info, ok := ix.sessions[id]
	if !ok {
		return false
	}
	info.ExpireAt = expireAt
	ix.sessions[id] = info
	return true
}

// setAttached 更新会话的 attached 状态（manager 在 attach/detach 时调用）。
func (ix *memoryIndex) setAttached(id string, attached bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	info, ok := ix.sessions[id]
	if !ok {
		return
	}
	info.Attached = attached
	ix.sessions[id] = info
}

// Remove 从索引移除会话（幂等）。
func (ix *memoryIndex) Remove(id string) {
	ix.mu.Lock()
	delete(ix.sessions, id)
	ix.mu.Unlock()
}

// Get 按 ID 取会话元数据副本。
func (ix *memoryIndex) Get(id string) (SessionInfo, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	info, ok := ix.sessions[id]
	return info, ok
}

// List 返回该用户全部会话元数据副本，按 createdAt 倒序。
func (ix *memoryIndex) List(username string) []SessionInfo {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]SessionInfo, 0, len(ix.sessions))
	for _, info := range ix.sessions {
		if info.Username == username {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// LongCount 返回该用户长期会话数量。
func (ix *memoryIndex) LongCount(username string) int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.longCountLocked(username)
}

func (ix *memoryIndex) longCountLocked(username string) int {
	n := 0
	for _, info := range ix.sessions {
		if info.Username == username && info.LongLived {
			n++
		}
	}
	return n
}

// TotalCount 返回该用户会话总数（长+短）。
func (ix *memoryIndex) TotalCount(username string) int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.totalCountLocked(username)
}

func (ix *memoryIndex) totalCountLocked(username string) int {
	n := 0
	for _, info := range ix.sessions {
		if info.Username == username {
			n++
		}
	}
	return n
}

// Extend 在索引锁内原子地延期一个长期会话：校验归属、长期、未过期，然后
// expireAt += hours。不触碰本地会话。
func (ix *memoryIndex) Extend(id, username string, hours int) (SessionInfo, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	info, ok := ix.sessions[id]
	if !ok || info.Username != username {
		return SessionInfo{}, ErrSessionNotFound
	}
	if !info.LongLived {
		return SessionInfo{}, ErrNotLongLived
	}
	if info.expired(ix.nowLocked()) {
		return SessionInfo{}, ErrSessionExpired
	}
	info.ExpireAt = info.ExpireAt.Add(time.Duration(hours) * time.Hour)
	ix.sessions[id] = info
	return info, nil
}

func (ix *memoryIndex) nowLocked() time.Time {
	if ix.now == nil {
		return time.Now()
	}
	return ix.now()
}

// ReapOnce 巡检一次：在索引锁内取过期快照，释放锁后逐个调用 closeFn（manager.Close）。
// 返回本次判定为过期的会话数。
func (ix *memoryIndex) ReapOnce() int {
	ix.mu.Lock()
	now := ix.nowLocked()
	var ids []string
	for id, info := range ix.sessions {
		if info.expired(now) {
			ids = append(ids, id)
		}
	}
	cf := ix.closeFn
	ix.mu.Unlock()

	for _, id := range ids {
		if cf != nil {
			cf(id)
		}
	}
	return len(ids)
}

// runReaper 周期性巡检，直到 stop 关闭。
func (ix *memoryIndex) runReaper(stop <-chan struct{}) {
	t := time.NewTicker(ix.reaperInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ix.ReapOnce()
		}
	}
}
