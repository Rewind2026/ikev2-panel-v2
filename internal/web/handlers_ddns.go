// DDNS 面板 API (v2-82 新增)
//
// 设计见 docs/design.md §19 + docs/release-notes-v2.82.md
//
// 路由:
//   - GET  /api/ddns/status  返回 {enabled, last_sync, failed}
//   - POST /api/ddns/toggle  切换开关,form: enabled=true|false
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// handleDDNSConfig v2.86-pr23a:POST /api/ddns/config,改 RR / EnableA / EnableAAAA / Period。
//
// Form 参数:
//   - rr             主机记录(空 = "@")
//   - enable_a       "true"/"1" 勾选 / 不传或 "false" 不勾
//   - enable_aaaa    "true"/"1" 勾选 / 不传或 "false" 不勾
//   - period_seconds 同步周期(10-3600)
//
// 副作用:
//   - 写 /data/panel-state/ddns.conf (atomic)
//   - 内存立即生效(Sync.SetConfig)
//   - 写审计
//   - flash 成功 → 302 /
//
// 不需要 DDNS Sync 实例吗?需要 — 没启用 DDNS 时改配置是无意义的(状态文件写了
// 但没人跑),所以走 /api/ddns/config 也要先校验 Sync 已初始化。
func (s *Server) handleDDNSConfig(w http.ResponseWriter, r *http.Request) {
	if s.DDNSSync == nil {
		http.Error(w, "DDNS not configured", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	rr := strings.TrimSpace(r.PostFormValue("rr"))
	enableA := parseBoolForm(r.PostFormValue("enable_a"))
	enableAAAA := parseBoolForm(r.PostFormValue("enable_aaaa"))

	periodStr := strings.TrimSpace(r.PostFormValue("period_seconds"))
	var periodSec int
	if periodStr != "" {
		n, err := strconv.Atoi(periodStr)
		if err != nil {
			http.Error(w, "period_seconds 必须是整数", http.StatusBadRequest)
			return
		}
		if n < 10 || n > 3600 {
			http.Error(w, "period_seconds 必须在 10-3600 之间", http.StatusBadRequest)
			return
		}
		periodSec = n
	}

	// RR 校验(独立于 panelstate — 这里直接给 Sync,Sync 自己会 fallback 到 "@")
	if rr != "" && rr != "@" {
		if !validRRChars(rr) {
			http.Error(w, "rr 不合法 (只允许字母/数字/_/-,1-63 字符)", http.StatusBadRequest)
			return
		}
	}

	if err := s.DDNSSync.SetConfig(rr, enableA, enableAAAA, periodSec); err != nil {
		s.Logger.Error("ddns config save failed", "err", err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.Logger.Info("ddns config updated",
		"rr", rr, "enable_a", enableA, "enable_aaaa", enableAAAA, "period_seconds", periodSec,
	)
	// v2.86-pr23a:审计
	s.writeAudit(r, "ddns.config.update",
		fmt.Sprintf("rr=%s,enable_a=%v,enable_aaaa=%v,period_seconds=%d",
			rr, enableA, enableAAAA, periodSec))

	// flash 成功
	sess, _ := SessionFrom(r.Context())
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{
			Message: "DNS 同步配置已保存",
		})
	}

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDDNSSyncNow v2.86-pr23a:POST /api/ddns/sync-now,触发后台立即 tick。
//
// 行为:
//   - 非阻塞:Sync.TriggerNow() 立即返回(handler 不等 tick 完成)
//   - flash 提示"已触发同步,刷新页面查看结果"
//   - 用户刷新后能在 ddns-card 看到新的 LastSync 时间戳 / V4IP / V6IP
//
// 节流:节流照常生效 — 刚同步过就 skip,所以多次点击不会撞 alidns 限流。
func (s *Server) handleDDNSSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.DDNSSync == nil {
		http.Error(w, "DDNS not configured", http.StatusNotFound)
		return
	}

	s.DDNSSync.TriggerNow()
	s.Logger.Info("ddns manual sync triggered")
	s.writeAudit(r, "ddns.sync.now", "")

	sess, _ := SessionFrom(r.Context())
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{
			Message: "已触发同步,刷新页面查看结果",
		})
	}

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDDNSFetchRemote v2.86-pr23a:POST /api/ddns/fetch-remote,阻塞调阿里云
// DescribeDomainRecords 拿 A + AAAA 当前实际值,跟 LastSync 对比排查。
//
// 行为:
//   - 阻塞(handler 等阿里云响应,通常 < 1s,加了 5s timeout 兜底)
//   - 结果存到 Sync.remoteSnap → home template 渲染展示
//   - flash 提示"已查询:A=1.2.3.4 / AAAA=2001:db8::1"(成功)或错误
//
// 注意:这是 **诊断** 操作,会**读**阿里云 API(RAM 权限需要 alidns:DescribeDomainRecords,
// 跟 DDNS 同步用的同一套)。不会修改任何记录。
func (s *Server) handleDDNSFetchRemote(w http.ResponseWriter, r *http.Request) {
	if s.DDNSSync == nil {
		http.Error(w, "DDNS not configured", http.StatusNotFound)
		return
	}

	// 5s timeout 兜底 — 阿里云正常 < 1s,挂死就报错
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	snap, err := s.DDNSSync.FetchRemote(ctx)
	if err != nil {
		s.Logger.Warn("ddns fetch remote failed", "err", err)
	}

	s.writeAudit(r, "ddns.fetch_remote",
		fmt.Sprintf("domain=%s,a=%s,aaaa=%s,err=%q", snap.Domain, snap.A, snap.AAAA, snap.Error))

	// flash 成功 / 失败都用 flash 提示(handler 端不直接渲染,统一回首页)
	sess, _ := SessionFrom(r.Context())
	if sess != nil && s.FlashStore != nil {
		var msg string
		if snap.Error != "" {
			msg = fmt.Sprintf("查询阿里云记录失败: %s", snap.Error)
		} else {
			parts := []string{}
			if snap.A != "" {
				parts = append(parts, "A="+snap.A)
			}
			if snap.AAAA != "" {
				parts = append(parts, "AAAA="+snap.AAAA)
			}
			if len(parts) == 0 {
				msg = fmt.Sprintf("阿里云 %s 当前没有 A/AAAA 记录", snap.Domain)
			} else {
				msg = fmt.Sprintf("阿里云 %s 当前: %s", snap.Domain, strings.Join(parts, " / "))
			}
		}
		s.FlashStore.Set(sess.ID, Flash{
			Message: msg,
		})
	}

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		// 序列化为简单 JSON,只暴露 A/AAAA/Error
		out := struct {
			OK     bool   `json:"ok"`
			A      string `json:"a,omitempty"`
			AAAA   string `json:"aaaa,omitempty"`
			Error  string `json:"error,omitempty"`
		}{
			OK:    err == nil,
			A:     snap.A,
			AAAA:  snap.AAAA,
			Error: snap.Error,
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// parseBoolForm v2.86-pr23a:把 form "true"/"1"/"on"/"" 解析成 bool。
//
// HTML checkbox 不勾选时**根本不发这个字段**,所以空字符串就当 false。
func parseBoolForm(v string) bool {
	v = strings.TrimSpace(v)
	return v == "true" || v == "1" || v == "on"
}

// validRRChars v2.86-pr23a:校验 RR 字符集 — 跟 ddns.allRRChars / panelstate.validRR 同 pattern。
func validRRChars(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
