// DDNS 面板 API (v2-82 新增)
//
// 设计见 docs/design.md §19 + docs/release-notes-v2.82.md
//
// 路由:
//   - GET  /api/ddns/status  返回 {enabled, last_sync, failed}
//   - POST /api/ddns/toggle  切换开关,form: enabled=true|false
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// DDNSStatusResp GET /api/ddns/status 返回。
type DDNSStatusResp struct {
	// Configured DDNS 同步器是否配置(nil 时 = false,即"未启用此功能")
	Configured bool `json:"configured"`
	// Enabled 当前是否启用
	Enabled bool `json:"enabled"`
	// Family v2-84:当前 family 配置(v4 / v6 / dual)
	Family string `json:"family"`
	// LastSync 最近一次同步结果(v2-84:per-family 独立字段)
	LastSync struct {
		Time    string `json:"time"`    // ISO8601 (空 = 从未同步)
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
		NewIP   string `json:"new_ip,omitempty"`
		OldIP   string `json:"old_ip,omitempty"`
		// v2-84 per-family 字段
		V4IP    string `json:"v4_ip,omitempty"`
		V4Error string `json:"v4_error,omitempty"`
		V6IP    string `json:"v6_ip,omitempty"`
		V6Error string `json:"v6_error,omitempty"`
	} `json:"last_sync"`
	// Failed 是否最近一次失败(true 时面板显示告警横幅)
	Failed bool `json:"failed"`
}

// handleDDNSStatus GET /api/ddns/status
func (s *Server) handleDDNSStatus(w http.ResponseWriter, r *http.Request) {
	resp := DDNSStatusResp{}
	if s.DDNSSync == nil {
		// DDNS 未配置(没启用此功能)
		resp.Configured = false
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	resp.Configured = true
	resp.Enabled = s.DDNSSync.IsEnabled()
	resp.Family = s.DDNSSync.Family()

	last := s.DDNSSync.LastSyncSnapshot()
	if !last.Time.IsZero() {
		// v2.85-PR3:用 s.DisplayTimezone 渲染 "2006-01-02 15:04:05 MST" 替代原 UTC + "Z"。
		// 面板用户看 DDNS 时间跟用户列表/最后登录时间格式一致。
		loc, err := time.LoadLocation(s.DisplayTimezone)
		if err != nil || loc == nil {
			loc = time.UTC
		}
		resp.LastSync.Time = last.Time.In(loc).Format("2006-01-02 15:04:05 MST")
		resp.LastSync.Success = last.Success
		resp.LastSync.Error = last.Error
		resp.LastSync.NewIP = last.NewIP
		resp.LastSync.OldIP = last.OldIP
		resp.LastSync.V4IP = last.V4IP
		resp.LastSync.V4Error = last.V4Error
		resp.LastSync.V6IP = last.V6IP
		resp.LastSync.V6Error = last.V6Error
	}

	// failed = "最近一次未成功"(简化:实际是"上次失败且尚未成功")
	if !last.Time.IsZero() && !last.Success {
		resp.Failed = true
	} else if exists := lastDDNSFailedExists(); exists {
		// 兜底:goroutine 没起来时,文件存在也算失败
		resp.Failed = true
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleDDNSToggle POST /api/ddns/toggle
//
// Form 参数: enabled=true|false
//
// 副作用:更新 ddns.conf 状态文件,下次 goroutine tick 生效。
func (s *Server) handleDDNSToggle(w http.ResponseWriter, r *http.Request) {
	if s.DDNSSync == nil {
		http.Error(w, "DDNS not configured", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	enabledStr := r.PostFormValue("enabled")
	enabled := enabledStr == "true" || enabledStr == "1" || enabledStr == "on"

	if err := s.DDNSSync.SetEnabled(enabled); err != nil {
		s.Logger.Error("ddns toggle failed", "err", err, "enabled", enabled)
		http.Error(w, "failed to update state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.Logger.Info("ddns toggled", "enabled", enabled)
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "ddns.toggle", fmt.Sprintf("enabled=%v", enabled))

	// 重定向回首页(普通表单提交场景)
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDDNSFamily v2-84:POST /api/ddns/family,切换 DDNS family。
//
// Form 参数: family=v4|v6|dual
//
// 双重白名单校验(handler 端 + Sync.SetFamily 端),跟 SetEnabled 同模式。
// 副作用:更新 ddns.conf 状态文件,下次 goroutine tick 生效。
func (s *Server) handleDDNSFamily(w http.ResponseWriter, r *http.Request) {
	if s.DDNSSync == nil {
		http.Error(w, "DDNS not configured", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	family := r.PostFormValue("family")
	// 双重白名单:handler 端先校验 + SetFamily 内部再校验
	switch family {
	case "v4", "v6", "dual":
		// ok
	default:
		http.Error(w, "invalid family (supported: v4, v6, dual)", http.StatusBadRequest)
		return
	}

	if err := s.DDNSSync.SetFamily(family); err != nil {
		s.Logger.Error("ddns family change failed", "err", err, "family", family)
		http.Error(w, "failed to update state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.Logger.Info("ddns family changed", "family", family)
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "ddns.family", fmt.Sprintf("family=%s", family))

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// lastDDNSFailedExists 兜底:检查 LAST_DDNS_FAILED 文件是否存在。
//
// goroutine 没启动 / 还没 tick 时,内存里 lastSync 是 zero;
// 但配置文件可能存在(上次运行失败留下的)。这种情况也算"已知失败"。
func lastDDNSFailedExists() bool {
	// 路径跟 ddns.Config.LastFailedFile 默认值一致
	for _, p := range []string{"/data/le/LAST_DDNS_FAILED", "/etc/ikev2/ddns.failed"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
