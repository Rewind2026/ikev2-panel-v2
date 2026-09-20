// 用户管理 handlers：list / new / create / detail / reset-password / enable / disable / delete。
// 设计见 docs/design.md §7
package web

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// usernameRe 用户名校验：3-32 位 [a-z0-9_-]。
var usernameRe = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// usersListData 列表页模板数据。
type usersListData struct {
	PageMeta    // P1-B：内嵌 Page/Title/AdminUsername/CSRFToken
	Users       []*store.User
	NewPassword string // 仅创建后一次性显示
	Flash       string // 顶部提示（如"已重置密码"）
}

// usersNewData 新增表单模板数据。
type usersNewData struct {
	PageMeta   // P1-B
	Error      string
	Username   string
	Note       string
	SpeedLimit int
	ExpiresAt  string // YYYY-MM-DD
	Enabled    bool
}

// userDetailData 用户详情页模板数据。
type userDetailData struct {
	PageMeta    // P1-B
	User        *store.User
	NewPassword string // 重置密码后一次性显示
	Flash       string
	Error       string
}

// handleUsersList GET /users
func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.Logger.Error("list users", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// P1-A：消费 session-only flash（一次性,渲染后自动消失）
	// 防止密码 / 状态消息走 query string（泄露到浏览器历史/日志）
	var newPassword, msg string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.Consume(sess.ID); err == nil {
			newPassword = f.NewPassword
			msg = f.Message
		}
	}

	s.RenderPage(w, "users_list", usersListData{
		PageMeta:    PageMeta{Page: "users_list", Title: "用户管理", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Users:       users,
		NewPassword: newPassword,
		Flash:       msg,
	})
}

// handleUserNew GET /users/new
func (s *Server) handleUserNew(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	s.RenderPage(w, "user_new", usersNewData{
		PageMeta:   PageMeta{Page: "user_new", Title: "新增用户", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Enabled:    true,
		SpeedLimit: 10,
	})
}

// handleUserCreate POST /users
//
// 关键：DB 写 + 文件写 + swanctl reload 必须事务化
// （design §3.3 + architecture §6.2）：
//   - 文件写失败 → 回滚 DB
//   - swanctl reload 失败 → 删除文件 + 回滚 DB
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	data := usersNewData{
		PageMeta:   PageMeta{Page: "user_new", Title: "新增用户", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Enabled:    true,
		SpeedLimit: 10,
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	data.Username = r.PostFormValue("username")
	data.Note = r.PostFormValue("note")
	data.Enabled = r.PostFormValue("enabled") == "1"
	data.ExpiresAt = r.PostFormValue("expires_at")
	speedStr := r.PostFormValue("speed_limit_mbps")
	if speedStr != "" {
		if v, err := strconv.Atoi(speedStr); err == nil {
			data.SpeedLimit = v
		}
	}

	// 校验
	if !usernameRe.MatchString(data.Username) {
		data.Error = "用户名必须是 3-32 位小写字母、数字、下划线、连字符"
		w.WriteHeader(http.StatusUnprocessableEntity)
		// P1-B 后 user_new 走两段渲染:content + layout。直接 ExecuteTemplate
		// "user_new.html" 在 P1-B 找不到模板(只剩 user_new_content.html),导致
		// 422 + 空 body + Chrome 显示 "HTTP ERROR 422"。
		s.RenderPage(w, "user_new", data)
		return
	}
	if data.SpeedLimit < 0 || data.SpeedLimit > 1000 {
		data.Error = "限速必须在 0-1000 Mbps 之间（0 = 不限速）"
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.RenderPage(w, "user_new", data)
		return
	}
	if len(data.Note) > 200 {
		data.Error = "备注最多 200 字"
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.RenderPage(w, "user_new", data)
		return
	}

	// 解析过期时间
	var expiresAt int64
	if data.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02", data.ExpiresAt)
		if err != nil {
			data.Error = "过期时间格式错误（YYYY-MM-DD）"
			w.WriteHeader(http.StatusUnprocessableEntity)
			s.RenderPage(w, "user_new", data)
			return
		}
		expiresAt = t.Unix()
	}

	// 生成 12 位随机密码
	password, err := auth.GeneratePassword(12)
	if err != nil {
		s.Logger.Error("generate password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 写 DB
	u := &store.User{
		Username:       data.Username,
		Password:       password,
		Enabled:        data.Enabled,
		Note:           data.Note,
		SpeedLimitMbps: data.SpeedLimit,
		ExpiresAt:      expiresAt,
	}
	id, err := s.Store.CreateUser(r.Context(), u)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			data.Error = "用户名已存在"
			w.WriteHeader(http.StatusUnprocessableEntity)
			s.RenderPage(w, "user_new", data)
			return
		}
		s.Logger.Error("create user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 写 swanctl 子配置 + ReloadAll（原子组合,P0-4 串行化）：
	//   - WriteUserConfAndReload 内部用 reloadMu 串行化所有 reload 操作
	//   - 防止并发请求触发两次 swanctl --load-all 导致 SA 中断两次 / 文件中间态
	if err := s.Swanctl.WriteUserConfAndReload(r.Context(), u.Username, u.Password); err != nil {
		s.Logger.Error("write swanctl conf+reload, rolling back DB", "err", err, "user", u.Username)
		if delErr := s.Store.DeleteUser(r.Context(), id); delErr != nil {
			s.Logger.Error("rollback delete user failed", "err", delErr, "user", u.Username)
		}
		data.Error = fmt.Sprintf("写入 swanctl 配置失败：%v（请检查容器内 /etc/swanctl/conf.d 权限）", err)
		w.WriteHeader(http.StatusInternalServerError)
		s.RenderPage(w, "user_new", data)
		return
	}

	// 写限速文件（updown 脚本读取）—— 不动 swanctl 状态,单独调
	if err := s.Limiter.WriteLimitFile(u.Username, u.SpeedLimitMbps); err != nil {
		s.Logger.Error("write limit file, rolling back", "err", err, "user", u.Username)
		_ = s.Swanctl.RemoveUserConf(u.Username) // 不重载（已 reload 过一次,在前面 WriteUserConfAndReload 里）
		if delErr := s.Store.DeleteUser(r.Context(), id); delErr != nil {
			s.Logger.Error("rollback delete user failed", "err", delErr, "user", u.Username)
		}
		data.Error = fmt.Sprintf("写入限速文件失败：%v（请检查容器内 /var/lib/ikev2-panel/limits 权限）", err)
		w.WriteHeader(http.StatusInternalServerError)
		s.RenderPage(w, "user_new", data)
		return
	}

	// 成功 → 列表页 + 通过 session-only flash 一次性显示密码（P1-A）
	//
	// 不再走 ?flash_new= query string（泄露到 URL/日志/Referer），
	// 改为：handler Set flash → 302 redirect /users（无 query string）→
	// handleUsersList 渲染前 Consume → 模板拿到 NewPassword。
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{
			NewPassword: password,
			Message:     fmt.Sprintf("用户 %s 已创建", u.Username),
		})
	} else {
		// Fallback（理论上不会发生，flash store 必装）：记日志便于排查
		s.Logger.Warn("FlashStore not configured; password only visible in DB")
	}
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "user.create", fmt.Sprintf("username=%s,speed=%d", u.Username, u.SpeedLimitMbps))
	http.Redirect(w, r, "/users", http.StatusFound)
}

// handleUserDetail GET /users/{id}
func (s *Server) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// P1-A：消费 session-only flash（重置密码后会跳到这里,带密码显示）
	var newPassword, msg string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.Consume(sess.ID); err == nil {
			newPassword = f.NewPassword
			msg = f.Message
		}
	}

	s.RenderPage(w, "user_detail", userDetailData{
		PageMeta:    PageMeta{Page: "user_detail", Title: u.Username, PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		User:        u,
		NewPassword: newPassword,
		Flash:       msg,
	})
}

// handleUserResetPassword POST /users/{id}/reset-password
func (s *Server) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// sess 用于 P1-A：把新密码 set 到 session-only flash,不走 URL
	sess, _ := SessionFrom(r.Context())

	newPw, err := auth.GeneratePassword(12)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.Store.UpdateUserPassword(r.Context(), id, newPw); err != nil {
		s.Logger.Error("update password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 更新 swanctl 子配置 + ReloadAll（原子组合,P0-4 串行化）
	if err := s.Swanctl.WriteUserConfAndReload(r.Context(), u.Username, newPw); err != nil {
		s.Logger.Error("update swanctl conf+reload", "err", err)
		http.Error(w, "swanctl 写入或重载失败", http.StatusInternalServerError)
		return
	}

	// P1-A：密码通过 session-only flash 传递,不再走 URL query string
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{
			NewPassword: newPw,
			Message:     "密码已重置",
		})
	}
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "user.reset_password", fmt.Sprintf("user_id=%d,username=%s", id, u.Username))
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// handleUserEnable POST /users/{id}/enable
func (s *Server) handleUserEnable(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Store.SetUserEnabled(r.Context(), id, true); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// P1-A：状态消息走 flash 而非 URL
	if sess, _ := SessionFrom(r.Context()); sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: "已启用"})
	}
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "user.enable", fmt.Sprintf("user_id=%d", id))
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// handleUserDisable POST /users/{id}/disable
func (s *Server) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Store.SetUserEnabled(r.Context(), id, false); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// 强制下线（如果连接中）
	_ = s.Swanctl.Terminate(r.Context(), usernameFromID(r, s.Store, id))
	// P1-A：状态消息走 flash 而非 URL
	if sess, _ := SessionFrom(r.Context()); sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: "已停用"})
	}
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "user.disable", fmt.Sprintf("user_id=%d", id))
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// handleUserDelete POST /users/{id}/delete
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// 删除顺序：DB → 文件+reload（原子） → limit → terminate
	// DB 失败 → 不动文件
	// 文件+reload 失败 → 回滚 DB
	if err := s.Store.DeleteUser(r.Context(), id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// 文件 + reload 原子组合（P0-4）
	if err := s.Swanctl.RemoveUserConfAndReload(r.Context(), u.Username); err != nil {
		s.Logger.Error("remove swanctl conf+reload, rolling back DB", "err", err)
		// 重建 user 记录
		u.ID = 0
		_, _ = s.Store.CreateUser(r.Context(), u)
		http.Error(w, "删除文件失败", http.StatusInternalServerError)
		return
	}
	_ = s.Limiter.RemoveLimitFile(u.Username)
	_ = s.Swanctl.Terminate(r.Context(), u.Username)
	// P1-A：状态消息走 flash 而非 URL
	if sess, _ := SessionFrom(r.Context()); sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: fmt.Sprintf("用户 %s 已删除", u.Username)})
	}
	// v2.85-PR8(U11):审计
	s.writeAudit(r, "user.delete", fmt.Sprintf("user_id=%d,username=%s", id, u.Username))
	http.Redirect(w, r, "/users", http.StatusFound)
}

// handleUserDeleteConfirmPage GET /users/{id}/delete
//
// v2.85-PR8(U10):双确认 - 第一步 GET 渲染确认页,不触发实际删除。
// 关 JS / 屏幕阅读器 / 自动化测试都走标准 HTTP 流程,无 confirm() hack。
func (s *Server) handleUserDeleteConfirmPage(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	s.RenderPage(w, "user_delete_confirm", userDeleteConfirmData{
		PageMeta: PageMeta{Page: "user_delete_confirm", Title: "删除确认", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		User:     u,
	})
}

// userDeleteConfirmData 删除确认页模板数据。
type userDeleteConfirmData struct {
	PageMeta
	User *store.User
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// usernameFromID 用于 disable 时拿 username（terminate 用）。
func usernameFromID(r *http.Request, s *store.Store, id int64) string {
	u, err := s.GetUserByID(r.Context(), id)
	if err != nil {
		return ""
	}
	return u.Username
}

// 简单 url 编码（避免 import net/url）。
func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b = append(b, c)
		} else {
			b = append(b, '%', hex[c>>4], hex[c&0xF])
		}
	}
	return string(b)
}
