// 阿里云凭证卡 API (v2-83 新增)
//
// 路由:
//   - GET  /api/aliyun/status   返回当前凭证状态(已配置/未配置 + 来源)
//   - POST /api/aliyun/save     保存凭证(form: key_id, key_secret)
//   - POST /api/aliyun/clear    清除凭证
//
// 设计要点(评审 C 建议):
//   - 凭证字段 type="password",保存后只显示掩码(首4+...+尾4)
//   - 写文件后内存立即生效;DDNS 下次 tick 自动用新凭证(无需重启)
//   - acme.sh 重启容器才生效(DNS-01 续期是 cron 驱动,见 entrypoint.sh)
//
// v2.86-pr23l:UX 改造 —— save / clear 不再 http.Error 跳转显示纯文本,
// 跟 DDNS 一致:写 flash + 302 redirect 到 /,模板里 banner 就近内联显示。
// 这样表单提交后浏览器刷新回首页,用户停留在原卡片位置(配合 layout.html
// 的 scroll restore),无白屏、无原生错误页。
//
// 设计见 docs/design.md §19.6(v2-83)。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
//   - key_id       可空:已配置场景下空值 = 保留旧 ID(只换 Secret)
//   - key_secret   可空:空值 = 不更新 Secret(配合 key_id 也空 = 拒绝)
//
// v2.86-pr23l:放宽必填约束,适配「已配置 + 只换 Secret」场景:
//   - 已配置 + 仅 secret 非空:保留旧 key_id,仅更新 secret
//   - 已配置 + 两个都空:返回 flash 错误,提示至少填一项
//   - 未配置 + 两个都空:返回 flash 错误,提示都必填
//   - 两个都非空:全量替换(原行为)
//
// v2.86-pr23l:UX —— 表单提交路径不再 http.Error(纯文本 + 原生错误页),
// 改成写 flash → 302 redirect 回 / ,让模板里的 banner 就近展示。
// AJAX 请求(Accept: application/json)仍走 JSON 错误响应,不影响脚本调用。
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

	// 已配置场景:空字段 = 保留旧值
	existing, _ := s.PanelState.ReadAliyun()
	if keyID == "" && keySecret == "" {
		s.respondAliyunFlash(w, r, "error",
			"请至少填写 AccessKey ID 或 AccessKey Secret 中的一项")
		return
	}
	if keyID == "" && existing != nil {
		keyID = existing.KeyID
	}
	if keySecret == "" && existing != nil {
		keySecret = existing.KeySecret
	}

	if err := s.PanelState.WriteAliyun(panelstate.AliyunCreds{
		KeyID:     keyID,
		KeySecret: keySecret,
	}); err != nil {
		s.Logger.Error("aliyun creds save failed", "err", err)
		s.respondAliyunFlash(w, r, "error",
			"保存失败: "+err.Error())
		return
	}

	s.Logger.Info("aliyun creds saved via panel",
		"key_id_masked", panelstate.MaskKeyID(keyID),
	)
	// v2.85-PR8(U11):审计(只记录 KeyID 掩码,不记录 Secret)
	s.writeAudit(r, "aliyun.save", fmt.Sprintf("key_id_masked=%s", panelstate.MaskKeyID(keyID)))

	s.respondAliyunFlash(w, r, "success", "阿里云凭证已保存")
}

// handleAliyunClear POST /api/aliyun/clear
//
// 副作用:
//   - 删除 /data/panel-state/aliyun.creds
//   - 清内存缓存(DDNS 下次 tick 用 env fallback)
func (s *Server) handleAliyunClear(w http.ResponseWriter, r *http.Request) {
	if s.PanelState == nil {
		http.Error(w, "panel state not initialized", http.StatusInternalServerError)
		return
	}
	if err := s.PanelState.ClearAliyun(); err != nil {
		s.Logger.Error("aliyun creds clear failed", "err", err)
		s.respondAliyunFlash(w, r, "error", "清除失败: "+err.Error())
		return
	}
	s.Logger.Info("aliyun creds cleared via panel")
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "aliyun.clear", "")

	s.respondAliyunFlash(w, r, "success", "阿里云凭证已清除")
}

// respondAliyunFlash v2.86-pr23l:统一 aliyun save/clear 的响应出口。
//
// 行为:
//   - AJAX 请求(Accept: application/json 或 X-Requested-With)→ JSON 响应,
//     避免被前端 fetch 误判跳转。
//   - 普通表单提交 → 写 flash 到 session + 302 redirect 回 /
//     (由 layout.html 的 scroll-restore JS 恢复原位置 + 模板 banner 就近渲染)。
func (s *Server) respondAliyunFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		ok := kind != "error"
		// 4xx 表达业务校验失败,但仍走 JSON,前端用 resp.ok 判断。
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

	// 普通浏览器表单:flash + redirect
	//
	// v2.86-pr23l:用 "aliyun" slot 写 flash — 跟 ddns 默认 slot 分开,
	// 模板端 home_content.html 各自从不同 slot 消费,alipay banner 显示在
	// aliyun 表单按钮下方,ddns banner 显示在 ddns 表单下方,互不干扰。
	sess, _ := SessionFrom(r.Context())
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.SetFor(sess.ID, "aliyun", Flash{Message: msg, Kind: kind})
		s.Logger.Info("aliyun flash written", "kind", kind, "msg", msg, "sessionID", sess.ID[:8]+"...")
	} else {
		s.Logger.Warn("aliyun flash NOT written (no session/flashstore)", "sessNil", sess == nil, "storeNil", s.FlashStore == nil)
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// wantsJSON v2.86-pr23l:判断客户端是否期望 JSON 响应。
//
// 检测 Accept 头(application/json)或 X-Requested-With(XHR 标记)。
// 没标 Accept 时,浏览器表单默认是 Accept: text/html,…… → 返回 false,
// 走 flash+redirect 路径。
func wantsJSON(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return true
	}
	a := r.Header.Get("Accept")
	if a == "" {
		return false
	}
	return strings.Contains(a, "application/json")
}
