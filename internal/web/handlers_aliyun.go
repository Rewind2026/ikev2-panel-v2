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
// 见 docs/architecture.md §1（internal/panelstate）。
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
		http.Error(w, "面板状态存储未初始化", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
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
			"保存失败："+err.Error())
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
		http.Error(w, "面板状态存储未初始化", http.StatusInternalServerError)
		return
	}
	if err := s.PanelState.ClearAliyun(); err != nil {
		s.Logger.Error("aliyun creds clear failed", "err", err)
		s.respondAliyunFlash(w, r, "error", "清除失败："+err.Error())
		return
	}
	s.Logger.Info("aliyun creds cleared via panel")
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "aliyun.clear", "")

	s.respondAliyunFlash(w, r, "success", "阿里云凭证已清除")
}

// respondAliyunFlash v2.86-pr23l:统一 aliyun save/clear 的响应出口。
//
// 实现已收敛到 respondFlashSlot(见下),这里只固定住 "aliyun" 这个 slot。
func (s *Server) respondAliyunFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	s.respondFlashSlot(w, r, "aliyun", kind, msg, nil)
}

// respondFlashSlot v2.94:所有「保存 / 清除」类 handler 的**唯一**响应出口。
//
// 为什么要有这个统一出口(v2.94 修的问题):
//
//	每张配置卡各写各的响应方式,漏一处就是一处裸文本错误页 —— 用户点保存后
//	浏览器直接跳到 https://host/api/cert/save 显示
//	「保存失败: letsencrypt 模式必须填域名(IKEV2_DOMAIN)」,
//	表单内容全丢、没有返回路径。v2.94 之前 cert / subnet 两张卡就是这么写的。
//
// 两条路径:
//
//	1. AJAX / API 客户端(wantsJSON 为真)→ JSON。
//	   error / msg 两个键都给同一个值:error 是 aliyun/settings 一直在用的键,
//	   msg 是 cert/subnet 原有契约的键,两个都留着避免打断已有调用方。
//	   extra 用来补端点自己的字段(如 cert 的 restart_required)。
//	2. 普通表单提交 → 写 flash 到 session 的指定 slot + 302 回 /。
//	   模板在对应卡片内部就近渲染 banner,配合 layout.html 的
//	   initDetailsPersistence() 恢复卡片展开态和滚动位置,用户停在原地看到反馈。
func (s *Server) respondFlashSlot(w http.ResponseWriter, r *http.Request, slot, kind, msg string, extra map[string]any) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		// 4xx 表达业务校验失败,但仍走 JSON,前端用 resp.ok 判断。
		status := http.StatusOK
		ok := kind != "error"
		if !ok {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		body := map[string]any{
			"ok":    ok,
			"kind":  kind,
			"error": msg,
			"msg":   msg,
		}
		for k, v := range extra {
			body[k] = v
		}
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	// 普通浏览器表单:flash + redirect。
	//
	// slot 分开写是为了让每张卡的反馈落在自己的卡片里 —— ddns 默认 slot 顶部横幅、
	// aliyun / settings / cert / subnet 各在自己的 slot,互不覆盖。
	sess, _ := SessionFrom(r.Context())
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.SetFor(sess.ID, slot, Flash{Message: msg, Kind: kind})
	} else {
		s.Logger.Warn("card flash NOT written (no session/flashstore)",
			"slot", slot, "sessNil", sess == nil, "storeNil", s.FlashStore == nil)
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
