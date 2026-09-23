// v2.86-PR12.21:每用户 mobileconfig 覆盖项 handler。
//
// 路由:
//   - GET  /users/{id}/mobileconfig-options  返回 JSON: opts + defaults,前端表单渲染
//   - POST /users/{id}/mobileconfig-options  Form 提交,保存到 store.User.MobileConfigOpts
//
// 设计:
//   - GET 返回 200 + JSON,前端用 fetch 拉,填进表单
//   - POST 校验 opts.Validate(),失败 → 400 + 错误文案;成功 → 303 重定向到 user 详情页
//     (跟其它 POST handler 一致,看 handlers_users.go 的 reset-password / enable / disable)。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yourname/ikev2-panel-v2/internal/cert"
)

// mobileconfigOptionsResp GET 响应。
//
// 同时给前端:已保存的 opts + 全局默认值,前端能展示"未设置"vs"自定义"两种状态。
type mobileconfigOptionsResp struct {
	Username string                `json:"username"`
	Saved    *cert.MobileConfigOpts `json:"saved"`    // 当前 user.MobileConfigOpts 解析结果;nil = 用全局默认
	Defaults cert.MobileConfigOpts `json:"defaults"` // 渲染默认值,前端显示参考
}

// handleUserMobileconfigOptionsGet GET /users/{id}/mobileconfig-options
func (s *Server) handleUserMobileconfigOptionsGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireUserID(w, r)
	if !ok {
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	saved, err := cert.DecodeMobileConfigOpts(u.MobileConfigOpts)
	if err != nil {
		// opts 损坏:返回 saved=nil + defaults,前端提示用户重新保存
		s.Logger.Warn("mobileconfig opts decode", "user_id", id, "err", err)
	}
	resp := mobileconfigOptionsResp{
		Username: u.Username,
		Saved:    saved,
		Defaults: cert.DefaultMobileConfigOpts(),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleUserMobileconfigOptionsSave POST /users/{id}/mobileconfig-options
//
// Form 参数(全部 optional,空字符串 = 用全局默认):
//   - user_defined_name       连接显示名
//   - disconnect_on_sleep     "1" / "0" / 空
//   - nat_keepalive_enabled   "1" / "0" / 空
//   - nat_keepalive_interval  秒数(string → int)
//   - on_demand_enabled       "1" / "0" / 空
//   - on_demand_profile       preset 名(见 cert.ValidOnDemandProfiles)
//   - ondemand_ssid           家里 Wi-Fi SSID
//   - include_all_networks    "1" / "0" / 空
//   - exclude_local_networks  "1" / "0" / 空
//   - dns_servers             换行分隔(IP 一行一个)
//   - dead_peer_detection_rate None/Low/Medium/High
//   - clear                   "1" = 清空所有覆盖,回到全局默认
func (s *Server) handleUserMobileconfigOptionsSave(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireUserID(w, r)
	if !ok {
		return
	}
	if _, err := s.Store.GetUserByID(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}

	// clear=1 → 清空 overlay
	if r.Form.Get("clear") == "1" {
		if err := s.Store.UpdateUserMobileConfigOpts(r.Context(), id, ""); err != nil {
			s.Logger.Error("clear mobileconfig opts", "user_id", id, "err", err)
			http.Error(w, "清空失败", http.StatusInternalServerError)
			return
		}
		s.writeAudit(r, "user.mobileconfig_opts.clear", fmt.Sprintf("user_id=%d", id))
		http.Redirect(w, r, fmt.Sprintf("/users/%d?flash=%s", id, url.QueryEscape("已恢复全局默认")),
			http.StatusSeeOther)
		return
	}

	opts, err := parseMobileConfigOptsFromForm(&r.Form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate
	if err := opts.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 序列化存盘
	encoded, err := cert.EncodeMobileConfigOpts(opts)
	if err != nil {
		s.Logger.Error("encode mobileconfig opts", "user_id", id, "err", err)
		http.Error(w, "序列化失败", http.StatusInternalServerError)
		return
	}
	if err := s.Store.UpdateUserMobileConfigOpts(r.Context(), id, encoded); err != nil {
		s.Logger.Error("save mobileconfig opts", "user_id", id, "err", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	s.writeAudit(r, "user.mobileconfig_opts.save", fmt.Sprintf("user_id=%d,json_len=%d", id, len(encoded)))
	http.Redirect(w, r, fmt.Sprintf("/users/%d?flash=%s", id, url.QueryEscape("已保存。下次下载 mobileconfig 生效")),
		http.StatusSeeOther)
}

// parseMobileConfigOptsFromForm 从 form 值构造 *MobileConfigOpts。
//
// 返回 *MobileConfigOpts(IsZeroOpts 时也是非 nil,只是字段都 nil) + error。
// 用户传了任何字段就保存(即使是 false / 0);全部为空时返回全 nil 的 opts,
// encode 时会被 EncodeMobileConfigOpts 转成 "" 写回 DB。
func parseMobileConfigOptsFromForm(f *url.Values) (*cert.MobileConfigOpts, error) {
	opts := &cert.MobileConfigOpts{}

	if v := strings.TrimSpace(f.Get("user_defined_name")); v != "" {
		v2 := v
		opts.UserDefinedName = &v2
	}

	if v := strings.TrimSpace(f.Get("disconnect_on_sleep")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("disconnect_on_sleep: %w", err)
		}
		opts.DisconnectOnSleep = &b
	}
	if v := strings.TrimSpace(f.Get("nat_keepalive_enabled")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("nat_keepalive_enabled: %w", err)
		}
		opts.NATKeepaliveEnabled = &b
	}
	if v := strings.TrimSpace(f.Get("nat_keepalive_interval")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("nat_keepalive_interval 必须是整数,当前 %q", v)
		}
		opts.NATKeepaliveInterval = &n
	}
	if v := strings.TrimSpace(f.Get("on_demand_enabled")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("on_demand_enabled: %w", err)
		}
		opts.OnDemandEnabled = &b
	}
	if v := strings.TrimSpace(f.Get("on_demand_profile")); v != "" {
		opts.OnDemandProfile = cert.OnDemandProfile(v)
	}
	if v := strings.TrimSpace(f.Get("ondemand_ssid")); v != "" {
		opts.OnDemandSSID = v
	}
	if v := strings.TrimSpace(f.Get("include_all_networks")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("include_all_networks: %w", err)
		}
		opts.IncludeAllNetworks = &b
	}
	if v := strings.TrimSpace(f.Get("exclude_local_networks")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("exclude_local_networks: %w", err)
		}
		opts.ExcludeLocalNetworks = &b
	}
	if v := strings.TrimSpace(f.Get("dead_peer_detection_rate")); v != "" {
		opts.DeadPeerDetectionRate = v
	}

	// dns_servers:换行 / 逗号分隔
	if v := strings.TrimSpace(f.Get("dns_servers")); v != "" {
		// 同时接受 \n 和 , 分隔(管理员可能从配置文件 copy)
		v = strings.ReplaceAll(v, "\r\n", "\n")
		lines := strings.Split(v, "\n")
		var out []string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// 同一行多个 IP 用逗号
			for _, ip := range strings.Split(line, ",") {
				ip = strings.TrimSpace(ip)
				if ip != "" {
					out = append(out, ip)
				}
			}
		}
		opts.DNSServers = out
	}

	return opts, nil
}

// parseBool 接受 "1"/"true"/"yes"/"on"/"0"/"false"/"no"/"off",其他返回错误。
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("必须是 1/0/true/false,当前 %q", s)
	}
}