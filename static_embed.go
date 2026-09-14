package main

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

// staticSub 返回指向 static/ 子目录的文件系统，供 http.FileServer 使用。
func staticSub() (fs.FS, error) {
	return fs.Sub(staticFS, "static")
}
