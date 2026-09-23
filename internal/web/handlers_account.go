// /account 账号自服务：管理员修改自己的密码。
//
// 路由：
//   GET  /account               渲染修改密码表单
//   POST /api/account/password  处理表单提交
//
// 设计要点（v2.86-pr21）：
//   - 只有 admin 自助场景，没有"忘记密码"流程（那是 main.go 首次启动随机生成的）
//   - 必须先校验 old_password 走 auth.VerifyPassword（bcrypt cost=12）
//   - new_password 最低 8 位（用户决策 Q1），不强制复杂度（避免用户写到一半放弃）
//   - new_password == confirm_new_password 强校验
//   - new_password != old_password 拒绝（防误操作）
//   - 改完不强制清其他 session（用户决策 Q2），只通过 CSRF + 旧密码双重把关
//   - 写审计：admin.password.change（含 username + remote ip）
//   - flash：成功 → "密码已更新"；失败 → 表单上方红色 banner
package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// accountMinPasswordLen admin 自助改密的最低长度（Q1 决策）。
const accountMinPasswordLen = 8

// accountPageData 账号设置页模板数据。
type accountPageData struct {
	PageMeta
	Username    string // 回显当前 admin
	Error       string // 表单错误回显
	OldPassword string // 失败时回显旧密码（可选；这里不复用以免泄露中间态）
	NewPassword string
}

// handleAccountPage GET /account。渲染改密表单。
func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())

	s.RenderPage(w, r, http.StatusOK, "account", accountPageData{
		PageMeta: PageMeta{
			Page:         "account",
			Title:        "账号设置",
			PageKey:      "account",
			AdminUsername: admin.Username,
			CSRFToken:    csrfTokenOf(sess),
		},
		Username: admin.Username,
	})
}

// handleAccountChangePassword POST /api/account/password。
//
// 流程：
//  1. 解析表单（csrf_token / old_password / new_password / confirm_password）
//  2. CSRF 中间件已经校验 token，这里不再重复
//  3. 校验 new_password 长度 + 一致性
//  4. 重新查 admin（用 ctx 里的 admin.ID）→ 校验 old_password
//  5. 校验 new != old
//  6. bcrypt hash → Store.UpdateAdminPassword
//  7. 写审计 admin.password.change
//  8. flash "密码已更新" → 302 → /account（带 success flash）
func (s *Server) handleAccountChangePassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	oldPw := r.PostFormValue("old_password")
	newPw := r.PostFormValue("new_password")
	confirmPw := r.PostFormValue("confirm_password")

	// 1. 基础校验
	if newPw == "" || confirmPw == "" || oldPw == "" {
		s.renderAccountError(w, r, "所有字段都必须填写")
		return
	}
	if len(newPw) < accountMinPasswordLen {
		s.renderAccountError(w, r, "新密码至少 8 位")
		return
	}
	if newPw != confirmPw {
		s.renderAccountError(w, r, "两次输入的新密码不一致")
		return
	}
	if oldPw == newPw {
		s.renderAccountError(w, r, "新密码不能与旧密码相同")
		return
	}
	// 防 trivial 弱密码（仅去前后空白后判断，给用户一点提示不阻断）
	if strings.TrimSpace(newPw) == "" {
		s.renderAccountError(w, r, "新密码不能全部是空白字符")
		return
	}

	// 2. 取当前 admin
	admin, ok := AdminFrom(r.Context())
	if !ok || admin == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 3. 校验旧密码
	if err := auth.VerifyPassword(admin.PasswordHash, oldPw); err != nil {
		// 不区分"用户不存在"还是"密码错"，统一文案（防枚举）
		s.Logger.Warn("account change password: old password verify failed",
			"admin", admin.Username)
		s.renderAccountError(w, r, "旧密码错误")
		return
	}

	// 4. 重新读一次 admin（防止 ctx 里的 hash 是旧版本，且拿到最新 updated_at）
	fresh, err := s.Store.GetAdminByUsername(r.Context(), admin.Username)
	if err != nil {
		s.Logger.Error("get admin for password change", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 5. bcrypt hash（cost=12，约 240ms/次）
	newHash, err := auth.BaseHashPassword(newPw)
	if err != nil {
		s.Logger.Error("hash new password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 6. 写库
	if err := s.Store.UpdateAdminPassword(r.Context(), fresh.Username, newHash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "admin not found", http.StatusNotFound)
			return
		}
		s.Logger.Error("update admin password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 7. 审计（成功才记，失败由 logger 负责）
	s.writeAudit(r, "admin.password.change",
		"username="+fresh.Username+",remote="+auth.ClientIP(r))

	// 8. flash 成功 → 重定向到 /account
	sess2, _ := SessionFrom(r.Context())
	if sess2 != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess2.ID, Flash{
			Message: "密码已更新",
		})
	}

	s.Logger.Info("admin password changed",
		"admin", fresh.Username,
		"remote", auth.ClientIP(r))

	http.Redirect(w, r, "/account", http.StatusFound)
}

// renderAccountError 渲染 /account 表单失败态。
//
// 与 renderLoginError 思路一致：把表单回显到模板，但**不**回显密码字段
// （密码不应在 HTML 源码里出现二次）。422 让前端 banner 更醒目。
func (s *Server) renderAccountError(w http.ResponseWriter, r *http.Request, msg string) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())

	s.RenderPage(w, r, http.StatusUnprocessableEntity, "account", accountPageData{
		PageMeta: PageMeta{
			Page:          "account",
			Title:         "账号设置",
			PageKey:       "account",
			AdminUsername: admin.Username,
			CSRFToken:     csrfTokenOf(sess),
		},
		Username: admin.Username,
		Error:    msg,
	})
}
