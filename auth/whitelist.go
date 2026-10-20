package auth

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 登录白名单（独立配置文件）。
//
// 取代旧的「解析系统分组名单」方案：白名单来自一个可独立维护的文本文件，
// 管理员增删用户后无需重启服务——每次判定时惰性检测文件 (mtime, size)，
// 变化才重读重建快照，实现热更新。
//
// 三态语义：
//   - failOpen=true            → 恒放行（逃生阀）；
//   - path=="" && !failOpen    → 恒拒绝（默认 fail-closed）；
//   - 已配置但文件缺失/为空/全注释 → 全拒（fail-closed）。
//
// reload 失败时采取 fail-stale：保留上次成功快照继续裁决（既不降级为 fail-open，
// 也不全拒）；首次加载失败则 fail-closed（空名单，全拒）。

// wlWhitelistUserRegexp 白名单用户名格式校验（与 usernameRegexp 同口径）。
var wlWhitelistUserRegexp = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// wlSnapshot 是不可变的白名单快照；读路径通过 atomic.Pointer 无锁获取。
//
// users 为小写归一后的用户名集合（nil 表示 fail-closed 空集语义）；modTime/size
// 记录最近一次成功读取时文件的元信息，用于惰性变化检测；n 为有效用户数（日志用）；
// loadErr 为最近一次读取错误串（用于「同错不重复打日志」的状态迁移抑制）。
type wlSnapshot struct {
	users   map[string]struct{}
	modTime time.Time
	size    int64
	n       int
	loadErr string
}

// UserWhitelist 承载登录白名单的惰性热更新引擎。
//
// 无后台 goroutine：每次 Allowed 时 os.Stat 与快照 (ModTime, Size) 比对，
// 变化才在 mu 串行段内重读文件、构建新快照并原子替换；读路径无锁。
type UserWhitelist struct {
	path     string
	failOpen bool
	mu       sync.Mutex // 仅串行化 reload 段
	state    atomic.Pointer[wlSnapshot]
}

// newUserWhitelist 构造一个白名单。path 为空表示未配置；failOpen 为逃生阀。
func newUserWhitelist(path string, failOpen bool) *UserWhitelist {
	w := &UserWhitelist{path: path, failOpen: failOpen}
	if failOpen {
		log.Printf("warn: web users whitelist fail-open is enabled; ALL accounts will be allowed")
	}
	return w
}

// Allowed 判定用户名是否在白名单内（大小写不敏感、精确匹配）。
func (w *UserWhitelist) Allowed(username string) bool {
	if w.failOpen {
		return true
	}
	if w.path == "" {
		// 未配置且非 fail-open：默认 fail-closed，恒拒绝。
		return false
	}

	snap := w.state.Load()
	if fi, err := os.Stat(w.path); err == nil && snap != nil && snap.loadErr == "" &&
		snap.size == fi.Size() && snap.modTime.Equal(fi.ModTime()) {
		// 命中缓存：文件未变且上次读取成功。
		return wlMatch(snap, username)
	}

	// 文件变化（或首次加载 / 上次读取失败）：串行 reload。
	ns := w.reload()
	if ns == nil {
		if snap != nil {
			return wlMatch(snap, username)
		}
		return false
	}
	return wlMatch(ns, username)
}

// LoadOnce 立即加载一次白名单（供 main 预热/诊断）。返回最近一次读取错误（若有）。
func (w *UserWhitelist) LoadOnce() error {
	if w.failOpen || w.path == "" {
		return nil
	}
	ns := w.reload()
	if ns == nil {
		return errors.New("web users whitelist reload failed")
	}
	if ns.loadErr != "" {
		return errors.New(ns.loadErr)
	}
	return nil
}

// Start 启动白名单引擎。惰性方案下仅做一次预热 LoadOnce；
// 接口保留为未来引擎演进（如后台轮询）预留。
func (w *UserWhitelist) Start(ctx context.Context) {
	_ = w.LoadOnce()
}

// Stop 停止白名单引擎。惰性方案下无后台活动，幂等空操作。
func (w *UserWhitelist) Stop() {}

// reload 在 mu 串行段内重读文件并原子替换快照。
//   - 读取失败：fail-stale（保留上次成功快照），首次加载失败则 fail-closed 空名单；
//   - 读取成功：构建新快照；数量变化时记 info。
//
// 返回最新快照（永不为 nil）。
func (w *UserWhitelist) reload() *wlSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()

	old := w.state.Load()

	f, err := os.Open(w.path)
	if err == nil {
		var fi os.FileInfo
		fi, err = f.Stat()
		if err == nil {
			var data []byte
			data, err = io.ReadAll(f)
			if err == nil {
				_ = f.Close()
				users := parseWhitelist(data)
				ns := &wlSnapshot{
					users:   users,
					modTime: fi.ModTime(),
					size:    fi.Size(),
					n:       len(users),
				}
				if old == nil {
					log.Printf("info: web users whitelist loaded from %s: %d users", w.path, ns.n)
				} else if old.n != ns.n {
					log.Printf("info: web users whitelist reloaded from %s: %d users (was %d)", w.path, ns.n, old.n)
				}
				if ns.n == 0 {
					log.Printf("warn: web users whitelist %s is empty (deny all)", w.path)
				}
				w.state.Store(ns)
				return ns
			}
		}
		_ = f.Close()
	}

	// 读取失败。
	errStr := err.Error()
	if old != nil && old.loadErr == errStr {
		// 同错抑制：不重复打日志。
		return old
	}
	if old != nil {
		// fail-stale：保留上次成功快照继续裁决（既不全拒也不 fail-open）。
		log.Printf("error: web users whitelist reload %s: %v (keeping last good snapshot: %d users)", w.path, err, old.n)
		ns := &wlSnapshot{
			users:   old.users,
			modTime: old.modTime,
			size:    old.size,
			n:       old.n,
			loadErr: errStr,
		}
		w.state.Store(ns)
		return ns
	}
	// 首次加载失败：fail-closed（空名单，全拒）。
	log.Printf("error: web users whitelist load %s: %v (deny-all fallback)", w.path, err)
	ns := &wlSnapshot{users: map[string]struct{}{}, loadErr: errStr}
	w.state.Store(ns)
	return ns
}

// wlMatch 在快照中精确查找（大小写不敏感）。
func wlMatch(snap *wlSnapshot, username string) bool {
	if snap == nil || snap.users == nil {
		return false
	}
	_, ok := snap.users[strings.ToLower(username)]
	return ok
}

// parseWhitelist 解析白名单文件内容为小写归一后的用户名集合（纯函数，便于单测）。
//
// 规则：一行一用户名；整行以 # 开头为注释；空行忽略；每行 TrimSpace；
// 不支持行尾 # 注释；不匹配 ^[a-z_][a-z0-9_-]{0,31}$ 的非注释非空行视为
// 非法行，跳过并记一次聚合 warn；结果小写入 map（天然去重）。
func parseWhitelist(data []byte) map[string]struct{} {
	users := make(map[string]struct{})
	invalid := 0
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.ToLower(strings.TrimSpace(raw)) // TrimSpace 同时处理 CRLF
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !wlWhitelistUserRegexp.MatchString(line) {
			invalid++
			continue
		}
		users[line] = struct{}{}
	}
	if invalid > 0 {
		log.Printf("warn: web users whitelist: skipped %d invalid line(s)", invalid)
	}
	return users
}
