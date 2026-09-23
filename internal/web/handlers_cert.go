// 证书配置卡 API (v2.86-PR12.5 新增)
//
// 路由:
//   - GET  /api/cert/status   返回当前证书配置(模式/域名/CN/邮箱/来源)
//   - POST /api/cert/save     保存证书配置(form: cert_mode, domain, server_cn, acme_email)
//   - POST /api/cert/clear    清除证书配置(回到 env 默认)
//
// 设计要点:
//   - cert.conf 是"配置"(域名/模式),不是"凭证",**不需要 audit + mask**
//     (跟 aliyun.creds 区分)
//   - Validate 在 WriteCertConfig 内部完成(handler 只透传 error)
//   - 写文件后,新配置要 **重启容器** 才生效(原因见 panelstate/cert.go 注释)
//     handler 返回时附 restart_required=true 提示用户
//   - GET /api/cert/status 用 Server.CertConfigStore(在 main.go 注入)
//   - 跟 cfg.ServerCN 不同步:ServerCN 在 cfg 启动时已固定(handler 看到的
//     server 字段是 cfg 启动时的值,不在运行时改)
//
// 设计见 docs/design.md §19.6(v2-83) + v2.86-PR12.5 扩展。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// CertStatusResp GET /api/cert/status 返回。
type CertStatusResp struct {
	// Configured 文件是否存在
	Configured bool `json:"configured"`
	// Mode 当前生效模式:"self-signed" / "letsencrypt"
	Mode string `json:"mode"`
	// Domain 域名(LE 模式必填)
	Domain string `json:"domain,omitempty"`
	// ServerCN mobileconfig RemoteIdentifier
	ServerCN string `json:"server_cn,omitempty"`
	// ACMEEmail LE 注册邮箱
	ACMEEmail string `json:"acme_email,omitempty"`
	// UpdatedAt unix seconds,list mode 的更新时间
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// Source 来源描述 panelstate / env-default / env-override / ""
	//   - panelstate: 面板 UI 填的(运行时改,需重启容器)
	//   - env-default: 完全用 env(用户没在面板改过)
	//   - env-override: env 覆盖了某字段(panelstate 文件存在但某字段为空)
	Source string `json:"source"`
	// RestartRequired 改完必须重启容器(返回 hint 给 UI)
	RestartRequired bool `json:"restart_required"`
}

// handleCertStatus GET /api/cert/status
func (s *Server) handleCertStatus(w http.ResponseWriter, r *http.Request) {
	resp := CertStatusResp{
		RestartRequired: false,
	}

	// 1) panelstate 优先
	if s.CertConfigStore != nil && s.CertConfigStore.CertConfigExists() {
		cfg, err := s.CertConfigStore.ReadCertConfig()
		if err == nil && cfg != nil {
			resp.Configured = true
			resp.Mode = cfg.CertMode
			resp.Domain = cfg.Domain
			resp.ServerCN = cfg.ServerCN
			resp.ACMEEmail = cfg.ACMEEmail
			resp.UpdatedAt = cfg.UpdatedAt
			resp.Source = "panelstate"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
	}

	// 2) env fallback(从 cfg 字段读,因为 cfg 在启动时已经把 panelstate 覆盖了 env)
	resp.Mode = s.CertMode
	if s.CertMode == "letsencrypt" {
		// Server.Domain 没注入到 Server struct(只有 cfg.Domain),这里通过 s 看下
		// 但 CertMode 有,Domain 没——简化方案:ServerCN 在 s.ServerCN 里。
		// Domain 从 cfg 没法取(handler 不直接拿 cfg),fallback 从 s.ServerCN 推不出来。
		// 实际场景:env-only 模式下 cert.conf 不存在,用户压根没在面板设过,
		// → Domain 是空的(否则 panelstate 早就写了)。
		// 真正生产用 LE 必须配 panelstate,env-only 不推荐。
		resp.Domain = "" // panelstate 没设时显示空,提示用户去面板配
	}
	// ServerCN 在 s.ServerCN(handler 已注入)
	resp.ServerCN = s.ServerCN
	if resp.Mode != "" {
		resp.Source = "env-default"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleCertSave POST /api/cert/save
//
// Form 参数:
//   - cert_mode    必填 (self-signed / letsencrypt)
//   - domain       LE 模式必填,自签模式可空
//   - server_cn    可空,空时 fallback 到 domain 或 ikev2.local
//   - acme_email   可空
//
// 副作用:
//   - 写 /data/panel-state/cert.conf (atomic rename, 0600)
//   - 返回 restart_required=true 提示用户重启容器
//   - 写审计(只记 mode/domain/cn/email,不算敏感凭证)
func (s *Server) handleCertSave(w http.ResponseWriter, r *http.Request) {
	if s.CertConfigStore == nil {
		http.Error(w, "cert config store not initialized", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	cfg := panelstate.CertConfig{
		CertMode:  r.PostFormValue("cert_mode"),
		Domain:    r.PostFormValue("domain"),
		ServerCN:  r.PostFormValue("server_cn"),
		ACMEEmail: r.PostFormValue("acme_email"),
	}

	if err := s.CertConfigStore.WriteCertConfig(cfg); err != nil {
		s.Logger.Warn("cert config save failed", "err", err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	masked := fmt.Sprintf("mode=%s domain=%q server_cn=%q acme_email=%q",
		cfg.CertMode, cfg.Domain, cfg.ServerCN, cfg.ACMEEmail)
	s.Logger.Info("cert config saved via panel", "masked", masked)
	s.writeAudit(r, "cert.save", masked)

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"restart_required":true,"msg":"保存成功,需重启容器才能生效(证书/域名变更需要重启 charon)"}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleCertClear POST /api/cert/clear
//
// 副作用:
//   - 删除 /data/panel-state/cert.conf
//   - 重启后 Go 进程从 env 读 CertMode/Domain/CN
func (s *Server) handleCertClear(w http.ResponseWriter, r *http.Request) {
	if s.CertConfigStore == nil {
		http.Error(w, "cert config store not initialized", http.StatusInternalServerError)
		return
	}
	if err := s.CertConfigStore.ClearCertConfig(); err != nil {
		s.Logger.Error("cert config clear failed", "err", err)
		http.Error(w, "清除失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Logger.Info("cert config cleared via panel")
	s.writeAudit(r, "cert.clear", "")

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"restart_required":true,"msg":"已清除,需重启容器回到 env 默认配置"}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}