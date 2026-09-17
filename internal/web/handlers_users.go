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
	AdminUsername string
	Users         []*store.User
	NewPassword   string // 仅创建后一次性显示
	Flash         string // 顶部提示（如"已重置密码"）
	CSRFToken     string // nav + 删除表单（§4.1 CSRF）
}

// usersNewData 新增表单模板数据。
type usersNewData struct {
	AdminUsername string
	Error         string
	Username      string
	Note          string
	SpeedLimit    int
	ExpiresAt     string // YYYY-MM-DD
	Enabled       bool
	CSRFToken     string // 创建表单（§4.1 CSRF）
}

// userDetailData 用户详情页模板数据。
type userDetailData struct {
	AdminUsername string
	User          *store.User
	NewPassword   string // 重置密码后一次性显示
	Flash         string
	Error         string
	CSRFToken     string // nav + 启停/重置/删除（§4.1 CSRF）
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
	_ = s.Templates.ExecuteTemplate(w, "users_list.html", usersListData{
		AdminUsername: admin.Username,
		Users:         users,
		CSRFToken:     csrfTokenOf(sess),
	})
}

// handleUserNew GET /users/new
func (s *Server) handleUserNew(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	_ = s.Templates.ExecuteTemplate(w, "user_new.html", usersNewData{
		AdminUsername: admin.Username,
		Enabled:       true,
		SpeedLimit:    10,
		CSRFToken:     csrfTokenOf(sess),
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
	data := usersNewData{AdminUsername: admin.Username, Enabled: true, SpeedLimit: 10, CSRFToken: csrfTokenOf(sess)}

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
		_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
		return
	}
	if data.SpeedLimit < 0 || data.SpeedLimit > 1000 {
		data.Error = "限速必须在 0-1000 Mbps 之间（0 = 不限速）"
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
		return
	}
	if len(data.Note) > 200 {
		data.Error = "备注最多 200 字"
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
		return
	}

	// 解析过期时间
	var expiresAt int64
	if data.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02", data.ExpiresAt)
		if err != nil {
			data.Error = "过期时间格式错误（YYYY-MM-DD）"
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
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
			_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
			return
		}
		s.Logger.Error("create user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 写 swanctl 子配置（容器内目录；本地 dev 可能没权限，记日志但不阻塞）
	if err := s.Swanctl.WriteUserConf(u.Username, u.Password); err != nil {
		s.Logger.Error("write swanctl conf, rolling back DB", "err", err, "user", u.Username)
		if delErr := s.Store.DeleteUser(r.Context(), id); delErr != nil {
			s.Logger.Error("rollback delete user failed", "err", delErr, "user", u.Username)
		}
		data.Error = fmt.Sprintf("写入 swanctl 配置失败：%v（请检查容器内 /etc/swanctl/conf.d 权限）", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
		return
	}

	// 写限速文件（updown 脚本读取）
	if err := s.Limiter.WriteLimitFile(u.Username, u.SpeedLimitMbps); err != nil {
		s.Logger.Error("write limit file, rolling back", "err", err, "user", u.Username)
		_ = s.Swanctl.RemoveUserConf(u.Username)
		if delErr := s.Store.DeleteUser(r.Context(), id); delErr != nil {
			s.Logger.Error("rollback delete user failed", "err", delErr, "user", u.Username)
		}
		data.Error = fmt.Sprintf("写入限速文件失败：%v（请检查容器内 /var/lib/ikev2-panel/limits 权限）", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
		return
	}

	// Reload
	if err := s.Swanctl.ReloadAll(r.Context()); err != nil {
		s.Logger.Error("swanctl --load-all failed, rolling back", "err", err, "user", u.Username)
		_ = s.Swanctl.RemoveUserConf(u.Username)
		if delErr := s.Store.DeleteUser(r.Context(), id); delErr != nil {
			s.Logger.Error("rollback delete user failed", "err", delErr, "user", u.Username)
		}
		data.Error = fmt.Sprintf("swanctl --load-all 失败：%v（已回滚）", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = s.Templates.ExecuteTemplate(w, "user_new.html", data)
		return
	}

	// 成功 → 列表页 + flash 一次性显示密码
	// 注意：password 通过 query string 传 → 列表页 flash
	http.Redirect(w, r, fmt.Sprintf("/users?flash_new=%s", urlEncode(password)), http.StatusFound)
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
	_ = s.Templates.ExecuteTemplate(w, "user_detail.html", userDetailData{
		AdminUsername: admin.Username,
		User:          u,
		NewPassword:   r.URL.Query().Get("flash_pw"),
		Flash:         r.URL.Query().Get("flash"),
		CSRFToken:     csrfTokenOf(sess),
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

	// 更新 swanctl 子配置（写到同一个文件）
	if err := s.Swanctl.WriteUserConf(u.Username, newPw); err != nil {
		s.Logger.Error("update swanctl conf", "err", err)
		http.Error(w, "swanctl 写入失败", http.StatusInternalServerError)
		return
	}
	if err := s.Swanctl.ReloadAll(r.Context()); err != nil {
		s.Logger.Error("swanctl reload", "err", err)
		// 已写文件 + reload 失败：用户用新密码连不上
		http.Error(w, "swanctl reload 失败", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/users/%d?flash_pw=%s&flash=%s", id, urlEncode(newPw), urlEncode("密码已重置")), http.StatusFound)
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
	http.Redirect(w, r, fmt.Sprintf("/users/%d?flash=%s", id, urlEncode("已启用")), http.StatusFound)
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
	http.Redirect(w, r, fmt.Sprintf("/users/%d?flash=%s", id, urlEncode("已停用")), http.StatusFound)
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

	// 删除顺序：DB → 文件 → reload
	// DB 失败 → 不动文件
	// 文件失败 → 回滚 DB
	if err := s.Store.DeleteUser(r.Context(), id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.Swanctl.RemoveUserConf(u.Username); err != nil {
		s.Logger.Error("remove swanctl conf, rolling back DB", "err", err)
		// 重建 user 记录
		u.ID = 0
		_, _ = s.Store.CreateUser(r.Context(), u)
		http.Error(w, "删除文件失败", http.StatusInternalServerError)
		return
	}
	_ = s.Limiter.RemoveLimitFile(u.Username)
	_ = s.Swanctl.Terminate(r.Context(), u.Username)
	if err := s.Swanctl.ReloadAll(r.Context()); err != nil {
		s.Logger.Error("swanctl reload after delete", "err", err)
	}
	http.Redirect(w, r, "/users?flash="+urlEncode(fmt.Sprintf("用户 %s 已删除", u.Username)), http.StatusFound)
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
