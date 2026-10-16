package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"webshell/audit"
	"webshell/auth"
	"webshell/handler"
)

func main() {
	// config.yaml 路径优先级：-config flag > 环境变量 CONFIG_PATH > 默认 ./config.yaml。
	configPath := flag.String("config", "", "path to config.yaml")
	flag.Parse()

	if *configPath == "" {
		*configPath = os.Getenv("CONFIG_PATH")
	}
	if *configPath == "" {
		*configPath = "./config.yaml"
	}

	// 加载 AD/token 配置（缺关键项时给出清晰错误并退出，fail-fast）。
	cfg, err := auth.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error (path=%s): %v", *configPath, err)
	}

	authr := auth.NewAuthenticator(cfg)

	// 静态资源托管：/static/* -> 内嵌 FS 的 static 子目录（不依赖磁盘路径/cwd）。
	sub, err := staticSub()
	if err != nil {
		log.Fatalf("static embed sub failed: %v", err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// 根路径返回入口页面（从内嵌 FS 读取），并设置 CSP / Cache-Control 头。
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		index, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "index not found", http.StatusInternalServerError)
			return
		}
		// CSP 裁决：vendored xterm.js 运行时通过 document.createElement("style") 注入样式，
		// 严格 style-src 'self' 会拦截致终端渲染异常，故放行 'unsafe-inline'（仅样式面）；
		// 脚本面仍为 script-src 'self' 封闭，XSS 增量低。内联 style="" 已改为 .hidden class，
		// 运行时动态样式与 CSSOM 赋值（如 element.style.x=）不受 style-src 限制。
		// 录屏回放：vendored asciinema-player 内嵌 WASM 做 VT 渲染（WebAssembly.instantiate），
		// 会被 script-src 'self' 拦截并抛 CompileError，故在 script-src 追加 wasm 编译放行关键字；
		// 该关键字不放开任意 eval/内联脚本，脚本面维持封闭，XSS 增量低。
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})

	// 登录：POST /api/login（签发 mgmt_token，并返回会话相关配置）。
	http.HandleFunc("/api/login", handler.Login(authr, cfg))

	// 会话索引（元数据/TTL/归属）+ 本地会话管理器（持有 pty）+ 一次性 ticket 存储。
	sessIndex := handler.NewIndex(0, 0, nil)
	mgr := handler.NewManager(sessIndex, nil)
	tickets := auth.NewTicketStore()
	requireMgmt := handler.RequireMgmt(authr.MgmtStore())

	// 优雅停机根 ctx：SIGINT/SIGTERM 触发；所有后台清理 goroutine 均以此为父。
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 存储清理：ticket 30s、mgmt 5min ticker（DESIGN_SESSIONS.md §4.1）。
	tickets.StartCleanup(shutdownCtx, 30*time.Second)
	authr.MgmtStore().StartCleanup(shutdownCtx, 5*time.Minute)

	// 审计存储（DESIGN_AUDIT §6）：启用时初始化、启动巡检、接线活跃会话解析器与清扫。
	var auditStore *audit.Store
	if cfg.AuditEnabled {
		auditStore, err = audit.NewStore(cfg.AuditDir, audit.Options{
			RecordInput:     cfg.AuditRecordIn,
			PasswordMode:    cfg.AuditPasswordMode,
			MaxSessionBytes: cfg.AuditMaxBytes,
			MaxTotalBytes:   cfg.AuditTotalBytes,
			// cfg.AuditRetention 为 time.Duration（0=不清理）；Options.RetentionDays 为整天数。
			RetentionDays: int(cfg.AuditRetention / (24 * time.Hour)),
			// EVW 输入间边界窗口：raw 输入按窗口 reached 决定明文/掩码，hang 兜底。
			EchoVerifyTimeout:     cfg.AuditEchoVerifyTimeout,
			EchoVerifyHangPolicy:  cfg.AuditEchoVerifyHangPolicy,
			EchoVerifyStrictBatch: cfg.AuditEchoVerifyStrictBatch,
			EchoVerifyTUI:         cfg.AuditEchoVerifyTUI,
		})
		if err != nil {
			log.Fatalf("audit store init: %v", err)
		}
		// 启动巡检：为上次异常终止的残留 cast 补写结束态（幂等）。
		if err := auditStore.ScanAndFinalize(); err != nil {
			log.Printf("audit startup scan: %v", err)
		}
		auditStore.SetActiveResolver(func() map[string]bool {
			m := make(map[string]bool)
			for _, id := range mgr.ActiveSessionIDs() {
				m[id] = true
			}
			return m
		})
		if cfg.AuditCleanupIvl > 0 {
			auditStore.StartCleanup(shutdownCtx, cfg.AuditCleanupIvl)
		}
	}
	if auditStore != nil {
		st := auditStore
		mgr.SetAudit(func(meta audit.SessionMeta) (*audit.Recorder, error) { return st.Open(meta) },
			cfg.AuditFailClosed, cfg.AuditRecordIn, cfg.AuditPasswordMode, cfg.AuditPasswordMaskWhen,
			cfg.AuditEchoVerifyStrictBatch)
	}

	// WebSocket 端点（统一消费一次性 ticket，create/attach 分派）。
	http.HandleFunc("/ws", handler.NewWs(mgr, tickets, cfg))

	// 会话级 HTTP 操作（全部 Bearer mgmt_token）。
	http.Handle("/api/sessions", requireMgmt(handler.NewSessionsHandler(mgr)))
	http.Handle("/api/session/ticket", requireMgmt(handler.NewTicketHandler(mgr, tickets, cfg)))
	http.Handle("/api/session/extend", requireMgmt(handler.NewExtendHandler(mgr, cfg)))
	http.Handle("/api/session/close", requireMgmt(handler.NewCloseHandler(mgr)))
	http.Handle("/api/logout", requireMgmt(handler.NewLogoutHandler(authr.MgmtStore())))
	http.Handle("/api/resume", requireMgmt(handler.NewResumeHandler(mgr)))
	http.Handle("/api/upload", requireMgmt(handler.NewUploadHandler(mgr)))
	http.Handle("/api/upload-check", requireMgmt(handler.NewUploadCheckHandler(mgr)))

	// 审计 REST API（DESIGN_AUDIT §9）。auditStore 为 nil（禁用）时仍注册路由，
	// handler 内部统一 404，保持「禁用即 404」语义。
	auditAPI := handler.NewAuditAPI(mgr, auditStore, cfg)
	http.Handle("/api/audit/sessions", requireMgmt(handler.NewAuditSessionsHandler(auditAPI)))
	http.Handle("/api/audit/meta", requireMgmt(handler.NewAuditMetaHandler(auditAPI)))
	http.Handle("/api/audit/cast", requireMgmt(handler.NewAuditCastHandler(auditAPI)))
	http.Handle("/api/audit/download", requireMgmt(handler.RequireAuditor(cfg)(handler.NewAuditDownloadHandler(auditAPI))))

	addr := cfg.Listen // 默认 ":8080"，缺省时由 LoadConfig 补齐
	// 若显式设置环境变量 PORT，则优先覆盖 config.yaml 的 listen（保持容器化部署向后兼容）。
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	srv := &http.Server{Addr: addr}
	// 优雅停机（DESIGN_AUDIT §6.3）：收到信号后先关会话（录屏落盘结束态），再关 HTTP。
	go func() {
		<-shutdownCtx.Done()
		log.Printf("shutdown signal received; closing sessions")
		mgr.Shutdown("server shutdown")
		shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			log.Printf("http server shutdown: %v", err)
		}
	}()

	log.Printf("web shell server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}
