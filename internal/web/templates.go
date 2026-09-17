// 模板加载。
package web

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
)

// LoadTemplates 从 templatesDir 加载全部 *.html，并解析 layout 共享片段。
//
// 约定：
//   - layout.html 定义 {{define "layout"}}...
//   - 各页面 {{define "content"}}...{{end}}
//   - 入口：ExecuteTemplate(w, "layout", data)，layout 引用 {{template "content" .}}
//
// 当前实现：所有 .html 平铺 Parse 在同一个模板集合里。
// ExecuteTemplate 寻找 base layout 时直接用 layout。
func LoadTemplates(templatesDir string) (*template.Template, error) {
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		return nil, fmt.Errorf("read templates dir: %w", err)
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		paths = append(paths, filepath.Join(templatesDir, e.Name()))
	}

	if len(paths) == 0 {
		return nil, fmt.Errorf("no .html files in %s", templatesDir)
	}

	t, err := template.New("").ParseFiles(paths...)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return t, nil
}