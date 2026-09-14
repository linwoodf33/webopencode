package main

import (
	"flag"
	"log"
	"net/http"
	"os"

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

	// 根路径返回入口页面（从内嵌 FS 读取）。
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})

	// 登录：POST /api/login。
	http.HandleFunc("/api/login", handler.Login(authr, cfg.Opencode.Enabled))

	// 会话仓库：支持页面刷新后恢复同一终端。
	hub := handler.NewSessionHub()

	// WebSocket 端点（升级前校验一次性 token；或按 sessionID 恢复会话）。
	http.HandleFunc("/ws", handler.NewWs(authr, &cfg.Opencode, hub))

	// 恢复前探测：GET /api/resume?session=<id>&username=<user>。
	http.HandleFunc("/api/resume", handler.NewResumeHandler(hub))

	// 文件上传：POST /api/upload?session=<id>&username=<user>（multipart file）。
	http.HandleFunc("/api/upload", handler.NewUploadHandler(hub))

	// 上传前权限预检：GET /api/upload-check?session=<id>&username=<user>。
	http.HandleFunc("/api/upload-check", handler.NewUploadCheckHandler(hub))

	addr := cfg.Listen // 默认 ":8080"，缺省时由 LoadConfig 补齐
	// 若显式设置环境变量 PORT，则优先覆盖 config.yaml 的 listen（保持容器化部署向后兼容）。
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Printf("web shell server listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
