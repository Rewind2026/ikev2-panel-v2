// v2.86-PR12.22:管理员后台 mobileconfig 默认值设置 handler。
//
// 路由:
//   - GET  /admin/mobileconfig-defaults   渲染设置页(admin 全局默认值 + builtin fallback)
//   - POST /admin/mobileconfig-defaults   保存 admin defaults(写到 /data/panel-state/mobileconfig.defaults.json)
//   - POST /admin/mobileconfig-defaults/clear   清除 admin defaults(回 builtin)
//
// 设计:
//   - 跟 /admin/cert/save 风格一致:GET 渲染 + POST 写入 atomic JSON
//   - 写入路径不需要重启容器(handler 端每次 ReadMobileConfigDefaults 拿最新值)
//   - clear 走 POST 而非 GET,符合 "破坏性操作要 POST" 习惯
//   - 用户详情页的 "Advanced Options" 是每用户 overlay;这里是 admin 全局默认值。
//     两者职责分明,前端模板用 `<a>` 互相跳转
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yourname/ikev2-panel-v2/internal/cert"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// mobileconfigDefaultsData 渲染 /admin/mobileconfig-defaults 页的模板数据。
type mobileconfigDefaultsData struct {
	PageMeta

	// Current 当前 admin defaults(已 read from panelstate);nil = 用 builtin
	Current *panelstate.MobileConfigDefaults

	// Builtin cert.BaseMobileConfigDefaults() 的镜像,模板用作 "未设置" 的占位符参考
	Builtin cert.MobileConfigOpts

	// v2.86-pr23l:Flash 反馈改走 FlashStore session(跟 aliyun / ddns / users 一致),
	// 不再用 ?flash=...&error=... query string(避免密码泄露历史教训 + 跟其他页行为一致)。
	// Flash     操作结果(保存成功 / 清除成功)
	// FlashKind flash 类别("" / "info" / "success" / "error"),模板切 banner 配色
	// Error     保留兼容:旧 query string ?error=... 的错误信息(若有)。新流程不再设它。
	Flash     string
	FlashKind string
	Error     string
}

// handleAdminMobileconfigDefaults GET /admin/mobileconfig-defaults
//
// 渲染 admin defaults 设置页,展示当前值 + builtin 对比 + 表单。
//
// v2.86-pr23l:flash 反馈从 query string (?flash=...&error=...) 切到 FlashStore session,
// 跟 aliyun / ddns / users 等页面一致 ——
//
//	- 无 query string(避免密码泄露历史教训,见 flash.go 注释)
//	- 操作反馈由 banner 就近在表单上方显示
//	- 错误用 banner-danger(红色),成功用 banner-success(绿色)
func (s *Server) handleAdminMobileconfigDefaults(w http.ResponseWriter, r *http.Request) {
	admin, ok := AdminFrom(r.Context())
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sess, _ := SessionFrom(r.Context())

	var current *panelstate.MobileConfigDefaults
	if s.MobileConfigDefaults != nil {
		d, err := s.MobileConfigDefaults.ReadMobileConfigDefaults()
		if err != nil {
			s.Logger.Warn("admin: read mobileconfig defaults", "err", err)
		}
		current = d
	}

	// v2.86-pr23l:消费上一步操作写入的 flash(默认 slot)。
	// 默认 slot 跟 aliyun / ddns / users 共享同一套,handler 写时都用 Set(),
	// 渲染时都用 Consume(),无需引入新 slot。
	var flashMsg, flashKind string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.Consume(sess.ID); err == nil {
			flashMsg = f.Message
			flashKind = f.Kind
		}
	}

	data := mobileconfigDefaultsData{
		PageMeta: PageMeta{
			Page:         "admin_mobileconfig_defaults",
			Title:        "Mobileconfig 默认值",
			PageKey:      "admin",
			AdminUsername: admin.Username,
			CSRFToken:     csrfTokenOf(sess),
			Breadcrumb: []Breadcrumb{
				{Label: "首页", Href: "/"},
				{Label: "Mobileconfig 默认值", Href: ""},
			},
		},
		Current:   current,
		Builtin:   cert.BaseMobileConfigDefaults(),
		Flash:     flashMsg,
		FlashKind: flashKind,
		// Error 字段保留兼容 — 旧 ?error= query string 仍能渲染(模板 line 20 还在用)。
		// 新流程(POST → flash session)不再设它。
		Error: r.URL.Query().Get("error"),
	}
	s.RenderPage(w, r, http.StatusOK, "admin_mobileconfig_defaults", data)
}

// handleAdminMobileconfigDefaultsSave POST /admin/mobileconfig-defaults
//
// 接收 12 个 form 字段(全部 optional),构造 panelstate.MobileConfigDefaults,
// 走 Validate() → atomic write 到 JSON 文件。
//
// 跟 /users/{id}/mobileconfig-options 的区别:
//   - user overlay 接受 OnDemandSSID(per-network);admin defaults 不收 SSID
//     (SSID 是 per-user 的)
//   - admin defaults 字段都是指针类型(*bool / *int),"未设置"用 nil 表示;
//     user overlay 也用指针,语义一致
//
// v2.86-pr23l:反馈从 query string 切到 FlashStore session ——
//
//	- 错误 → kind="error"(红色 banner-danger)
//	- 成功 → kind="success"(绿色 banner-success)
//	- 写完 flash 后 302 redirect 到无 query string 的页面,GET handler 渲染 banner
func (s *Server) handleAdminMobileconfigDefaultsSave(w http.ResponseWriter, r *http.Request) {
	if s.MobileConfigDefaults == nil {
		http.Error(w, "mobileconfig defaults store not initialized", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}

	d, err := parseMobileConfigDefaultsFromForm(&r.Form)
	if err != nil {
		s.Logger.Warn("admin: parse mobileconfig defaults", "err", err)
		s.writeMCDefaultsFlash(w, r, "error", "保存失败: "+err.Error())
		return
	}

	if err := s.MobileConfigDefaults.WriteMobileConfigDefaults(d); err != nil {
		s.Logger.Warn("admin: write mobileconfig defaults", "err", err)
		s.writeMCDefaultsFlash(w, r, "error", "保存失败: "+err.Error())
		return
	}

	s.writeAudit(r, "admin.mobileconfig_defaults.save",
		fmt.Sprintf("user_defined_name=%q disconnect_on_sleep=%v nat_keepalive_enabled=%v nat_keepalive_interval=%d on_demand_enabled=%v on_demand_profile=%q include_all_networks=%v exclude_local_networks=%v dns_count=%d dead_peer_detection_rate=%q",
			d.UserDefinedName, boolPtrStr(d.DisconnectOnSleep), boolPtrStr(d.NATKeepaliveEnabled),
			intPtrVal(d.NATKeepaliveInterval, 0), boolPtrStr(d.OnDemandEnabled),
			d.OnDemandProfile, boolPtrStr(d.IncludeAllNetworks),
			boolPtrStr(d.ExcludeLocalNetworks), len(d.DNSServers),
			d.DeadPeerDetectionRate))

	s.writeMCDefaultsFlash(w, r, "success", "已保存。下次用户下载 mobileconfig 立即生效,无需重启容器")
}

// writeMCDefaultsFlash v2.86-pr23l:统一 save/clear 的 flash + 302 出口。
//
// 行为:
//   - XHR / Accept: application/json → JSON 响应(给将来脚本调用留口子,本次未启用)
//   - 普通表单 → 写 flash 到 session + 302 redirect 回 /admin/mobileconfig-defaults
func (s *Server) writeMCDefaultsFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		ok := kind != "error"
		status := http.StatusOK
		if !ok {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    ok,
			"kind":  kind,
			"error": msg,
		})
		return
	}

	sess, _ := SessionFrom(r.Context())
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: msg, Kind: kind})
	}
	http.Redirect(w, r, "/admin/mobileconfig-defaults", http.StatusFound)
}

// handleAdminMobileconfigDefaultsClear POST /admin/mobileconfig-defaults/clear
//
// 清除 admin defaults,回 builtin 出厂值。
//
// 设计选择:不需要 confirm(用户已经在 UI 上点了"恢复出厂值"按钮,
// 跟 /admin/cert/clear 风格一致)。
//
// v2.86-pr23l:跟 save 一致,反馈改走 FlashStore session。
func (s *Server) handleAdminMobileconfigDefaultsClear(w http.ResponseWriter, r *http.Request) {
	if s.MobileConfigDefaults == nil {
		http.Error(w, "mobileconfig defaults store not initialized", http.StatusInternalServerError)
		return
	}
	if err := s.MobileConfigDefaults.ClearMobileConfigDefaults(); err != nil {
		s.Logger.Error("admin: clear mobileconfig defaults", "err", err)
		s.writeMCDefaultsFlash(w, r, "error", "清除失败: "+err.Error())
		return
	}
	s.writeAudit(r, "admin.mobileconfig_defaults.clear", "")
	s.writeMCDefaultsFlash(w, r, "success", "已恢复 builtin 出厂值")
}

// parseMobileConfigDefaultsFromForm 从 form 构造 panelstate.MobileConfigDefaults。
//
// 所有字段 optional;空字符串 / 缺字段 = nil(让 builtin 生效)。
// "未设置"和"显式 false"用 *bool 区分:parseBool 成功后无论 true / false 都构造 *bool。
//
// 复用 cert.MobileConfigOpts.Validate 校验逻辑(通过 panelstate.MobileConfigDefaults.Validate)。
func parseMobileConfigDefaultsFromForm(f *url.Values) (panelstate.MobileConfigDefaults, error) {
	d := panelstate.MobileConfigDefaults{}

	if v := strings.TrimSpace(f.Get("user_defined_name")); v != "" {
		d.UserDefinedName = v
	}
	if v := strings.TrimSpace(f.Get("disconnect_on_sleep")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return d, fmt.Errorf("disconnect_on_sleep: %w", err)
		}
		d.DisconnectOnSleep = &b
	}
	if v := strings.TrimSpace(f.Get("nat_keepalive_enabled")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return d, fmt.Errorf("nat_keepalive_enabled: %w", err)
		}
		d.NATKeepaliveEnabled = &b
	}
	if v := strings.TrimSpace(f.Get("nat_keepalive_interval")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return d, fmt.Errorf("nat_keepalive_interval 必须是整数,当前 %q", v)
		}
		d.NATKeepaliveInterval = &n
	}
	if v := strings.TrimSpace(f.Get("on_demand_enabled")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return d, fmt.Errorf("on_demand_enabled: %w", err)
		}
		d.OnDemandEnabled = &b
	}
	if v := strings.TrimSpace(f.Get("on_demand_profile")); v != "" {
		d.OnDemandProfile = v
	}
	if v := strings.TrimSpace(f.Get("include_all_networks")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return d, fmt.Errorf("include_all_networks: %w", err)
		}
		d.IncludeAllNetworks = &b
	}
	if v := strings.TrimSpace(f.Get("exclude_local_networks")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return d, fmt.Errorf("exclude_local_networks: %w", err)
		}
		d.ExcludeLocalNetworks = &b
	}
	if v := strings.TrimSpace(f.Get("dead_peer_detection_rate")); v != "" {
		d.DeadPeerDetectionRate = v
	}

	// dns_servers:换行 / 逗号分隔
	if v := strings.TrimSpace(f.Get("dns_servers")); v != "" {
		v = strings.ReplaceAll(v, "\r\n", "\n")
		lines := strings.Split(v, "\n")
		var out []string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			for _, ip := range strings.Split(line, ",") {
				ip = strings.TrimSpace(ip)
				if ip != "" {
					out = append(out, ip)
				}
			}
		}
		d.DNSServers = out
	}

	return d, nil
}

// boolPtrStr 格式化 *bool 给 audit log: nil → "<nil>" 否则 true/false。
func boolPtrStr(p *bool) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.FormatBool(*p)
}

// intPtrVal 读 *int,nil 时返回 fallback(0 给 audit log)。
func intPtrVal(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}