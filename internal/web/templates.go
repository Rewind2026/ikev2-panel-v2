// 模板加载。
package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// storeUser 别名,避免在模板 helper 里写完整 store.User (handler 用 *store.User)
// jsonOneClient 接受 *store.User。
type storeUser = store.User

// LoadTemplates 从 templatesDir 加载全部 *.html，并解析 layout 共享片段。
//
// P1-B layout 约定：
//   - layout.html 定义 {{define "layout"}}<html>...</html>{{end}}
//     layout 内部引用 {{template "content" .}} 和 {{template "nav" .}}
//   - nav.html   定义 {{define "nav"}}<nav>...</nav>{{end}}（可选,login 页可不 include）
//   - 各页面 {{define "content"}}...{{end}}{{template "layout" .}}
//
// 实现要点：
//   - layout.html 必须**最先** ParseFiles,
//     否则其他模板的 {{template "layout" .}} 找不到定义会 panic
//   - 用 template.New("").ParseFiles() 把所有模板合到一个集合,
//     让 {{define "content"}} 在每个子模板里独立存在（不会冲突）
//   - FuncMap 注册 formatTime / formatBytes 等,需在 ParseFiles 前 Funcs() 调用
//
// v2.85-PR3:新增 displayTZ 参数(IANA TZ 名,默认 UTC)。
//   - formatTime 表格列用:"2006-01-02 15:04 MST"(带时区缩写后缀)
//   - formatTimeDDNS DDNS/API 用:"2006-01-02 15:04:05 MST"
//   - 闭包捕获 tzName,模板调用语法不变:{{formatTime .X}}
//
// 历史：v2 之前所有模板平铺,nav 在 5 个页面重复。P1-B 后 nav 只在 layout.html 一处。
func LoadTemplates(templatesDir string, displayTZ string) (*template.Template, error) {
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

	// 确保 layout.html 第一个被 Parse（保证 {{define "layout"}} 在其他模板引用之前已注册）
	sort.SliceStable(paths, func(i, j int) bool {
		baseI := filepath.Base(paths[i])
		baseJ := filepath.Base(paths[j])
		if baseI == "layout.html" {
			return true
		}
		if baseJ == "layout.html" {
			return false
		}
		return baseI < baseJ
	})

	t := template.New("")
	t = t.Funcs(template.FuncMap{
		// v2.85-PR3:formatTime / formatTimeDDNS 用闭包捕获 displayTZ,
		// 模板调用语法不变({{formatTime .X}}),Go html/template FuncMap 值是
		// interface{} 不能直接传 string,只能闭包。
		"formatTime":     func(unix int64) string { return formatTimeTpl(unix, displayTZ) },
		"formatTimeDDNS": func(unix int64) string { return formatTimeDDNS(unix, displayTZ) },
		"formatBytes":    formatBytesTpl,
		// v2.85-PR8(U11):audit_log.timestamp 是 unix nano,用格式串 +07:00 显示本地时区
		"formatUnixNano": func(nano int64) string {
			if nano <= 0 {
				return "-"
			}
			loc, err := time.LoadLocation(displayTZ)
			if err != nil || loc == nil {
				loc = time.UTC
			}
			return time.Unix(0, nano).In(loc).Format("2006-01-02 15:04:05.000 MST")
		},
		// v2.86-PR12.20:parseAuditDetails 解析 "k=v k=v" 串为 [{K,V}, ...] 切片,
		// 供 audit_content.html 渲染 dl.kv-list 使用。
		//
		// 设计: Details 串可能包含空格分隔的 k=v, 但 v 里也可能含空格 (e.g. error msg).
		// 简化策略: 按空格 split 后, 第一个 token 用第一个 '=' 分 k 和 v; 后续 token 视为裸
		// value 拼到上一个 token 的 v 上 (空格分隔).
		// 这样对 `username=rewind,speed=10` (逗号分隔) 也兼容 — split by space 即可.
		"parseAuditDetails": parseAuditDetails,
		// pageContent 把 page 拼成 "<page>_content"（P1-B layout 路由用）
		// 注意：Go html/template 不允许在 {{template}} 指令里直接用
		//   {{template (printf "%s_content" .Page) .}}
		// 所以这里用 FuncMap 包装一层。
		"pageContent": pageContent,
		// v2.86-PR12.21: 拓扑 JSON 序列化器 (Go html/template 不能直接序列化 slice 到 data-clients attr)
		"jsonTopologyClients": jsonTopologyClients,
		"jsonOneClient":       jsonOneClient,
		// v2.86-PR12.21: audit 严重性映射 (timeline-dot class),基于 event name 启发式
		"auditSeverity": auditSeverity,
	})

	if _, err := t.ParseFiles(paths...); err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return t, nil
}

// formatTimeTpl v2.85-PR3:表格列用 "2006-01-02 15:04 MST"(带时区缩写后缀)。
//
// 注册到 FuncMap 后，模板里可以这样用：
//
//	<td>{{formatTime .User.CreatedAt}}</td>
//
// tzName 是 IANA TZ 名(默认 UTC);LoadLocation 失败时 fallback UTC,
// 容器仍能起来,跟 loadDisplayTimezone 策略一致。
func formatTimeTpl(unix int64, tzName string) string {
	if unix <= 0 {
		return "-"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	return time.Unix(unix, 0).In(loc).Format("2006-01-02 15:04 MST")
}

// formatTimeDDNS v2.85-PR3:DDNS/API 输出用 "2006-01-02 15:04:05 MST"。
//
// 跟 formatTimeTpl 区别:精确到秒(API 端需要),格式串带秒。
// 用法:{{formatTimeDDNS .LastSync}}。
func formatTimeDDNS(unix int64, tzName string) string {
	if unix <= 0 {
		return "-"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	return time.Unix(unix, 0).In(loc).Format("2006-01-02 15:04:05 MST")
}

// pageContent 返回 "<page>_content" 字符串,用于 layout 内 {{template pageContent .}} 调用。
//
// 这是绕开 Go html/template 不支持 {{template (printf ...) .}} 的限制。
// 每个子模板文件必须是 "_content.html" 后缀,layout 路由靠这个拼接。
func pageContent(page string) string {
	return page + "_content"
}

// formatBytesTpl 模板用：人类可读字节（B / KB / MB / GB）。
func formatBytesTpl(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	if n < k*k {
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	}
	if n < k*k*k {
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
}

// PageMeta 公共布局字段（P1-B）：所有 page data struct 内嵌这个,
//
// 即可在模板里用 .Page 选子模板、用 .Title 设 title。
//
// 用法：
//
//	type usersListData struct {
//	    PageMeta
//	    AdminUsername string
//	    ...
//	}
//
//	data := usersListData{
//	    PageMeta: PageMeta{Page: "users_list", Title: "用户管理"},
//	    AdminUsername: ...,
//	}
//
// BodyHTML 在 RenderPage 期间由子模板渲染填充(避免 Go html/template 同名 define 覆盖问题)。
type PageMeta struct {
	Page     string        // 选子模板,如 "users_list" → users_list_content.html
	Title    string        // 浏览器 title
	BodyHTML template.HTML // 子模板渲染结果（RenderPage 期间注入）

	// 公共字段（嵌入 PageMeta 的所有 page data 都自动有），
	// 这样 layout/nav 模板里用 {{.AdminUsername}} 不会因为某个 page data 没这字段而报错。
	AdminUsername string
	CSRFToken     string

	// v2.86-PR12.19:nav active 标记的短 key (e.g. "home" / "users" / "audit" / "login")
	PageKey string

	// v2.86-PR12.21:TopBar breadcrumb (e.g. [{首页, /}, {用户, /users}, {alice, ""}])
	// Href="" 表示当前页 (不可点击)。所有 page 都用,省略时不渲染。
	Breadcrumb []Breadcrumb
}

// Breadcrumb v2.86-PR12.21:TopBar 面包屑一项。
type Breadcrumb struct {
	Label string
	Href  string
}

// RenderPage 渲染完整页面（两段渲染模式 P1-B 最终方案）：
//  1. ExecuteTemplate("<page>_content.html", data)  →  bodyBuf
//  2. data.BodyHTML = template.HTML(bodyBuf.String())  （用反射注入）
//  3. ExecuteTemplate("layout", data)  →  w
//
// 关键：Go html/template 不支持 {{template (printf ...) .}} 动态选模板,
// 也不能用 FuncMap 函数当 template name,
// 所以只能**两段渲染**:先渲染子模板到 buffer,再把 buffer 注入 layout。
//
// 用法（推荐）：
//
//	data := usersListData{PageMeta: PageMeta{Page: "users_list", Title: "用户管理"}, ...}
//	s.RenderPage(w, "users_list", data)
func (s *Server) RenderPage(w http.ResponseWriter, page string, data interface{}) {
	// setBodyHTML 用 reflect.SetField 要求 data 是 non-nil pointer。
	// 如果 caller 传了值类型,这里自动取地址。
	v := reflect.ValueOf(data)
	if v.Kind() == reflect.Struct {
		// 自动包装成指针
		newData := reflect.New(v.Type())
		newData.Elem().Set(v)
		data = newData.Interface()
	}

	// Step 1: 渲染子模板到 buffer
	var bodyBuf bytes.Buffer
	if err := s.Templates.ExecuteTemplate(&bodyBuf, page+"_content.html", data); err != nil {
		if s.Logger != nil {
			s.Logger.Error("render content", "page", page, "err", err)
		}
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	// Step 2: 注入 BodyHTML（data 必须是指针,reflect 才能 SetField）
	if err := setBodyHTML(data, bodyBuf.Bytes()); err != nil {
		if s.Logger != nil {
			s.Logger.Error("set BodyHTML", "page", page, "err", err)
		}
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	// Step 3: 渲染 layout 外壳
	if err := s.Templates.ExecuteTemplate(w, "layout", data); err != nil {
		if s.Logger != nil {
			s.Logger.Error("render layout", "page", page, "err", err)
		}
	}
}

// setBodyHTML 通过反射把 body bytes 注入到 data.BodyHTML 字段。
//
// 因为 RenderPage 接受 interface{},具体类型不知道,只能用反射。
// data 必须有 BodyHTML 字段(直接字段或嵌入字段都可以)。
func setBodyHTML(data interface{}, body []byte) error {
	v := reflect.ValueOf(data)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return fmt.Errorf("data is not a non-nil pointer")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("data is not a struct")
	}

	// 1. 先看顶层是否有 BodyHTML 字段
	if f := v.FieldByName("BodyHTML"); f.IsValid() {
		if !f.CanSet() {
			return fmt.Errorf("BodyHTML field not settable")
		}
		f.Set(reflect.ValueOf(template.HTML(body)))
		return nil
	}

	// 2. 否则遍历嵌入字段,找 PageMeta.BodyHTML
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanAddr() {
			continue
		}
		// embedded field (Anonymous = true)
		if v.Type().Field(i).Anonymous && f.Kind() == reflect.Struct {
			embedded := f.FieldByName("BodyHTML")
			if embedded.IsValid() && embedded.CanSet() {
				embedded.Set(reflect.ValueOf(template.HTML(body)))
				return nil
			}
		}
	}

	return fmt.Errorf("data has no BodyHTML field (neither top-level nor in embedded PageMeta)")
}

// parseAuditDetails 把 audit Details 字段的 "k=v k=v ..." 格式字符串解析成 [{K,V}, ...] 切片。
//
// 例子:
//
//	"unique=1 remote=2408:... eap_id=rewind vip=[10.10.0.1]"
//	  → [{K:"unique", V:"1"}, {K:"remote", V:"2408:..."}, {K:"eap_id", V:"rewind"}, {K:"vip", V:"[10.10.0.1]"}]
//
// 策略: 按空格 split, 每个 token 看是否含 '=' — 有就当作 k=v 解析 (k = 第一个 '=' 左边,
// v = 第一个 '=' 右边整段); 否则追加到上一个 token 的 v (空格拼接, 兼容带空格的 value).
//
// v2.86-PR12.20:给 audit 详情列用, 取代 PR12.19 的 "k=v k=v" 一坨字符串. 模板里配合
// <dl class="kv-list">{{range parseAuditDetails .Details}}<dt>{{.K}}</dt><dd>{{.V}}</dd>{{end}}</dl>.
func parseAuditDetails(s string) []KV {
	if s == "" {
		return nil
	}
	tokens := strings.Fields(s)
	out := make([]KV, 0, len(tokens))
	for _, tok := range tokens {
		if i := strings.IndexByte(tok, '='); i >= 0 {
			out = append(out, KV{K: tok[:i], V: tok[i+1:]})
		} else if n := len(out); n > 0 {
			// 追加到上一个 token 的 value
			out[n-1].V += " " + tok
		} else {
			// 没有前导 token 的裸 token (畸形)
			out = append(out, KV{K: "?", V: tok})
		}
	}
	return out
}

// KV parseAuditDetails 返回的 key-value 对。
//
// v2.86-PR12.20:放在 templates.go, 仅模板层使用
type KV struct {
	K string
	V string
}

// TopologyClient v2.86-PR12.21: 拓扑画布客户端节点数据 (渲染成 SVG)。
//
// 用于模板 data-clients JSON attr。Home 传所有 enabled 用户 + 活跃 SA,
// UserDetail 只传当前用户 (data-focus-user)。
type TopologyClient struct {
	ID       int64  `json:"id,omitempty"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	BytesIn  int64  `json:"bytesIn,omitempty"`
	BytesOut int64  `json:"bytesOut,omitempty"`
	Href     string `json:"href,omitempty"`
}

// jsonTopologyClients 把客户端 slice 序列化为 JSON 字符串,嵌入 SVG data attr。
//
// 用 encoding/json 而不是 template.HTMLEscape,因为 data-clients 是 attribute value
// (包在 '' 或 "" 里),template 会自己处理 outer quote escaping。
//
// v2.86-PR12.21
func jsonTopologyClients(clients []TopologyClient) (template.JS, error) {
	if len(clients) == 0 {
		return template.JS("[]"), nil
	}
	b, err := jsonMarshalIndent(clients)
	if err != nil {
		return template.JS("[]"), err
	}
	return template.JS(b), nil
}

// jsonOneClient 单个用户转 JSON (UserDetail 页用)。
//
// v2.86-PR12.21
func jsonOneClient(u *storeUser) (template.JS, error) {
	if u == nil {
		return template.JS("{}"), nil
	}
	online := u.Enabled && u.LastUsedAt > 0
	c := TopologyClient{
		ID:       u.ID,
		Name:     u.Username,
		Online:   online,
		BytesIn:  u.BytesInTotal,
		BytesOut: u.BytesOutTotal,
		Href:     "/users/" + strconv.FormatInt(u.ID, 10),
	}
	b, err := jsonMarshalIndent([]TopologyClient{c})
	if err != nil {
		return template.JS("[]"), err
	}
	return template.JS(b), nil
}

// jsonMarshalIndent 缩进序列化 (前端 console.log 友好)。
func jsonMarshalIndent(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return string(b), nil // fallback 不缩进
	}
	return out.String(), nil
}

// auditSeverity v2.86-PR12.21: 把 audit event 映射到 timeline-dot class。
//
// 启发式 (event 名包含关键字):
//   - "delete" / "disable" / "fail"        → danger
//   - "update" / "reset" / "save" / "toggle" → warn
//   - "create" / "enable"                  → ok
//   - 其它 (system listener / 通用)         → info
//
// 让 audit log 时间线一眼看出严重性。
func auditSeverity(event string) string {
	e := strings.ToLower(event)
	switch {
	case strings.Contains(e, "delete"),
		strings.Contains(e, "disable"),
		strings.Contains(e, "fail"),
		strings.Contains(e, "error"):
		return "danger"
	case strings.Contains(e, "update"),
		strings.Contains(e, "reset"),
		strings.Contains(e, "save"),
		strings.Contains(e, "toggle"),
		strings.Contains(e, "expire"):
		return "warn"
	case strings.Contains(e, "create"),
		strings.Contains(e, "enable"),
		strings.Contains(e, "up"):
		return "ok"
	default:
		return "info"
	}
}