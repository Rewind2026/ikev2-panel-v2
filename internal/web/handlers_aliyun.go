// 阿里云凭证卡 API (v2-83 新增)
//
// 路由:
//   - GET  /api/aliyun/status   返回当前凭证状态(已配置/未配置 + 来源)
//   - POST /api/aliyun/save     保存凭证(form: key_id, key_secret, confirm=yes)
//   - POST /api/aliyun/clear    清除凭证
//
// 设计要点(评审 C 建议):
//   - 凭证字段 type="password",保存后只显示掩码(首4+...+尾4)
//   - 保存需二次确认(confirm=yes)防止误操作
//   - 写文件后内存立即生效;DDNS 下次 tick 自动用新凭证(无需重启)
//   - acme.sh 重启容器才生效(DNS-01 续期是 cron 驱动,见 entrypoint.sh)
//
// 设计见 docs/design.md §19.6(v2-83)。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// AliyunStatusResp GET /api/aliyun/status 返回。
type AliyunStatusResp struct {
	// Configured 文件是否存在
	Configured bool `json:"configured"`
	// KeyIDMasked 掩码后的 ID(首4+...+尾4)
	KeyIDMasked string `json:"key_id_masked,omitempty"`
	// Source 来源描述(panelstate / env-new / env-legacy-ddns / env-legacy-acme.sh)
	// Configured=false 时为空字符串
	Source string `json:"source"`
}

// handleAliyunStatus GET /api/aliyun/status
func (s *Server) handleAliyunStatus(w http.ResponseWriter, r *http.Request) {
	resp := AliyunStatusResp{
		Configured: false,
		Source:     s.AliyunAccessKeySource,
	}
	if s.PanelState != nil && s.PanelState.AliyunExists() {
		resp.Configured = true
		// 读取 ID 用于掩码展示(Secret 不展示)
		if c, err := s.PanelState.ReadAliyun(); err == nil && c != nil {
			resp.KeyIDMasked = panelstate.MaskKeyID(c.KeyID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAliyunSave POST /api/aliyun/save
//
// Form 参数:
//   - key_id       必填
//   - key_secret   必填
//   - confirm      必填 = "yes"(二次确认)
//
// 副作用:
//   - 写 /data/panel-state/aliyun.creds (atomic rename, 0600)
//   - 更新内存缓存(DDNS 下次 tick 用新凭证)
func (s *Server) handleAliyunSave(w http.ResponseWriter, r *http.Request) {
	if s.PanelState == nil {
		http.Error(w, "panel state not initialized", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	keyID := r.PostFormValue("key_id")
	keySecret := r.PostFormValue("key_secret")
	confirm := r.PostFormValue("confirm")

	if confirm != "yes" {
		http.Error(w, "需要 confirm=yes 二次确认才能保存", http.StatusBadRequest)
		return
	}

	if err := s.PanelState.WriteAliyun(panelstate.AliyunCreds{
		KeyID:     keyID,
		KeySecret: keySecret,
	}); err != nil {
		s.Logger.Error("aliyun creds save failed", "err", err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.Logger.Info("aliyun creds saved via panel",
		"key_id_masked", panelstate.MaskKeyID(keyID),
	)
	// v2.85-PR8(U11):审计(只记录 KeyID 掩码,不记录 Secret)
	s.writeAudit(r, "aliyun.save", fmt.Sprintf("key_id_masked=%s", panelstate.MaskKeyID(keyID)))

	// 重定向回首页(普通表单提交场景)
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"restart_required":true}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAliyunClear POST /api/aliyun/clear
//
// 副作用:
//   - 删除 /data/panel-state/aliyun.creds
//   - 清内存缓存(DDNS 下次 tick 用 env fallback)
//
// 不需要二次确认(用户主动点"清除"按钮,意图明确)。
func (s *Server) handleAliyunClear(w http.ResponseWriter, r *http.Request) {
	if s.PanelState == nil {
		http.Error(w, "panel state not initialized", http.StatusInternalServerError)
		return
	}
	if err := s.PanelState.ClearAliyun(); err != nil {
		s.Logger.Error("aliyun creds clear failed", "err", err)
		http.Error(w, "清除失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Logger.Info("aliyun creds cleared via panel")
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "aliyun.clear", "")

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
