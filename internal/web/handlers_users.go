// 用户管理 handlers：list / new / create / detail / reset-password / enable / disable / delete。
//
// 设计见 docs/design.md §7
//
// 重构(v2.86-PR13 重构):所有"会改变外部状态"的 IO 步骤(DB → swanctl conf → reload
// → limiter file → terminate)集中到 user_pipeline.go 的三个 pipeline 函数。
// 本文件只剩"参数解析 + 表单渲染 + flash + audit"等纯 HTTP 逻辑。
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
//
// v2.86-PR13.0 注释:这个限制是**业务必需**而非历史妥协——
// VPN 协议(EAP-MSCHAPv2 / swanctl secrets 块语法)对 username 字符集敏感,
// 详见 design §9.2。前端表单 placeholder 也明示"仅限小写字母/数字/下划线/连字符"。
var usernameRe = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// usersListData 列表页模板数据。
type usersListData struct {
	PageMeta    // P1-B：内嵌 Page/Title/AdminUsername/CSRFToken
	Users       []*store.User
	NewPassword string // 仅创建后一次性显示
	Flash       string // 顶部提示（如"已重置密码"）
	FlashKind   string // v2.86-pr23j:flash 语义(info/success/warn/error),决定 banner 配色
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
	// expiresAtUnix 解析后的 unix 时间戳,handler 写入 store.User 时使用。
	// 模板不引用(模板只用 ExpiresAt 字符串),所以不导出。
	expiresAtUnix int64
}

// userDetailData 用户详情页模板数据。
type userDetailData struct {
	PageMeta    // P1-B
	User        *store.User
	NewPassword string // 重置密码后一次性显示
	Flash       string
	FlashKind   string // v2.86-pr23j:flash 语义(info/success/warn/error),决定 banner 配色
	Error       string

	// v2.86-PR12.21:Visual-first topology + breadcrumb
	ServerAddr string
	NowUnix    int64
	Breadcrumb []Breadcrumb
	// EZZON 视觉标记:用户当前是否在线(看 SA 表)。dev 模式无 charon → 默认 false。
	UserOnline bool
}

// userDeleteConfirmData 删除确认页模板数据。
type userDeleteConfirmData struct {
	PageMeta
	User *store.User
}

// pageUsersList 用户列表页元数据常量。
const (
	pageUsersList      = "users_list"
	pageUserNew        = "user_new"
	pageUserDetail     = "user_detail"
	pageDeleteConfirm  = "user_delete_confirm"
	defaultSpeedMbps   = 10
	defaultPasswordLen = 12
	maxNoteChars       = 200
	maxSpeedMbps       = 1000
)

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
	var newPassword, msg, msgKind string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.Consume(sess.ID); err == nil {
			newPassword = f.NewPassword
			msg = f.Message
			msgKind = f.Kind
		}
	}

	s.RenderPage(w, r, http.StatusOK, pageUsersList, usersListData{
		PageMeta:    PageMeta{Page: pageUsersList, Title: "用户管理", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Users:       users,
		NewPassword: newPassword,
		Flash:       msg,
		FlashKind:   msgKind,
	})
}

// handleUserNew GET /users/new
func (s *Server) handleUserNew(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	s.RenderPage(w, r, http.StatusOK, pageUserNew, usersNewData{
		PageMeta:   PageMeta{Page: pageUserNew, Title: "新增用户", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Enabled:    true,
		SpeedLimit: defaultSpeedMbps,
	})
}

// handleUserCreate POST /users
//
// 关键：DB 写 + 文件写 + swanctl reload 必须事务化（design §3.3 + architecture §6.2）：
//   - 文件写失败 → 回滚 DB
//   - swanctl reload 失败 → 删除文件 + 回滚 DB
//   - limiter file 失败 → 反向 reload 删除用户 conf + 回滚 DB
//
// v2.86-PR13.0:全部 IO 步骤抽到 userPipelines.CreateUser,本 handler 只做参数解析/校验/渲染。
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// 表单解析 + 校验(任何校验失败 → 422 + 渲染表单带 Error)
	data, valid := parseAndValidateUserNewForm(r)
	if !valid {
		data.PageMeta = PageMeta{Page: pageUserNew, Title: "新增用户", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)}
		s.RenderPage(w, r, http.StatusUnprocessableEntity, pageUserNew, data)
		return
	}

	// 生成 12 位随机密码
	password, err := auth.GeneratePassword(defaultPasswordLen)
	if err != nil {
		s.Logger.Error("generate password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	u := &store.User{
		Username:       data.Username,
		Password:       password,
		Enabled:        data.Enabled,
		Note:           data.Note,
		SpeedLimitMbps: data.SpeedLimit,
		ExpiresAt:      data.expiresAtUnix,
	}

	// 走 pipeline:DB → swanctl → limiter,任一失败自动回滚
	res, err := s.newUserPipelines().CreateUser(r.Context(), u)
	if err != nil {
		s.Logger.Error("user create pipeline", "err", err, "user", u.Username)
		// pipeline 已回滚 DB,这里把 Error 反馈给用户
		data.PageMeta = PageMeta{Page: pageUserNew, Title: "新增用户", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)}
		data.Error = friendlyUserCreateError(err)
		s.RenderPage(w, r, http.StatusInternalServerError, pageUserNew, data)
		return
	}

	// P1-A：成功 → session-only flash 一次性显示密码
	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{
			NewPassword: res.Password,
			Message:     fmt.Sprintf("用户 %s 已创建", u.Username),
		})
	}
	s.writeAudit(r, "user.create", fmt.Sprintf("username=%s,speed=%d", u.Username, u.SpeedLimitMbps))
	http.Redirect(w, r, "/users", http.StatusFound)
}

// parseAndValidateUserNewForm 从 r 抽表单字段 + 校验,返回填充好的 usersNewData
// (含内部字段 expiresAtUnix) + 校验是否通过。
//
// 抽出来是为了让 handleUserCreate 主体只剩"渲染 + pipeline",校验失败时
// caller 拿到的 data.PageMeta 是占位空值,caller 自己覆盖。
func parseAndValidateUserNewForm(r *http.Request) (usersNewData, bool) {
	data := usersNewData{
		Enabled:    true,
		SpeedLimit: defaultSpeedMbps,
	}

	data.Username = r.PostFormValue("username")
	data.Note = r.PostFormValue("note")
	data.Enabled = r.PostFormValue("enabled") == "1"
	data.ExpiresAt = r.PostFormValue("expires_at")
	if speedStr := r.PostFormValue("speed_limit_mbps"); speedStr != "" {
		if v, err := strconv.Atoi(speedStr); err == nil {
			data.SpeedLimit = v
		}
	}

	if !usernameRe.MatchString(data.Username) {
		data.Error = "用户名必须是 3-32 位小写字母、数字、下划线、连字符"
		return data, false
	}
	if data.SpeedLimit < 0 || data.SpeedLimit > maxSpeedMbps {
		data.Error = fmt.Sprintf("限速必须在 0-%d Mbps 之间（0 = 不限速）", maxSpeedMbps)
		return data, false
	}
	if len(data.Note) > maxNoteChars {
		data.Error = fmt.Sprintf("备注最多 %d 字", maxNoteChars)
		return data, false
	}

	if data.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02", data.ExpiresAt)
		if err != nil {
			data.Error = "过期时间格式错误（YYYY-MM-DD）"
			return data, false
		}
		data.expiresAtUnix = t.Unix()
	}
	return data, true
}

// friendlyUserCreateError 把 pipeline 错误转成用户可见的中文提示。
//
// 注意:pipeline 内部错误已含技术细节(如 swanctl conf 路径),
// 这里只取**第一条**做文案,避免回显内部错误链。
func friendlyUserCreateError(err error) string {
	if errors.Is(err, errSwanctlWriteFailed) {
		return "写入 swanctl 配置失败（请检查容器内 /etc/swanctl/conf.d 权限）"
	}
	if errors.Is(err, errLimiterWriteFailed) {
		return "写入限速文件失败（请检查容器内 /var/lib/ikev2-panel/limits 权限）"
	}
	return "创建用户失败,请查看服务日志"
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

	var newPassword, msg, msgKind string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.Consume(sess.ID); err == nil {
			newPassword = f.NewPassword
			msg = f.Message
			msgKind = f.Kind
		}
	}

	// v2.86-pr23p:判断当前用户是否在线(主页 topology 是直接看 SA 表的;
	// 之前这里硬编码 false,导致用户详情页 topology 永远显示 disconnected,
	// 即使已经有流量产生 — 跟主页 topology 不一致)。
	//
	// 复用 loadActiveSAs (3s timeout + ESTABLISHED 过滤),遍历 RemoteID 跟
	// username 比对 —— parser.go 里 RemoteID 已经 extractID 剥过 "2 " / "CN="
	// 前缀,可以直接 == 。
	activeSAs, _ := loadActiveSAs(r.Context(), s)
	userOnline := false
	for _, sa := range activeSAs {
		if sa.RemoteID == u.Username {
			userOnline = true
			break
		}
	}

	s.RenderPage(w, r, http.StatusOK, pageUserDetail, userDetailData{
		PageMeta: PageMeta{Page: pageUserDetail, Title: u.Username, PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess),
			Breadcrumb: []Breadcrumb{{Label: "用户", Href: "/users"}, {Label: u.Username}}},
		User:        u,
		NewPassword: newPassword,
		Flash:       msg,
		FlashKind:   msgKind,
		ServerAddr:  s.ServerAddr,
		NowUnix:     time.Now().Unix(),
		UserOnline:  userOnline,
	})
}

// handleUserResetPassword POST /users/{id}/reset-password
//
// v2.86-PR13.0:走 pipeline,失败自动回滚 DB 密码到旧值。
func (s *Server) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, _ := SessionFrom(r.Context())

	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	oldPassword := u.Password

	newPw, err := auth.GeneratePassword(defaultPasswordLen)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	res, err := s.newUserPipelines().ResetPassword(r.Context(), id, oldPassword, newPw)
	if err != nil {
		s.Logger.Error("user reset password pipeline", "err", err, "user_id", id)
		http.Error(w, "重置密码失败（已自动回滚 DB 状态）", http.StatusInternalServerError)
		return
	}

	if sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{
			NewPassword: res.NewPassword,
			Message:     "密码已重置",
		})
	}
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
	if sess, _ := SessionFrom(r.Context()); sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: "已启用"})
	}
	s.writeAudit(r, "user.enable", fmt.Sprintf("user_id=%d", id))
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// handleUserDisable POST /users/{id}/disable
//
// v2.86-PR13.0:走 pipeline,Terminate 失败时自动回滚 enabled=true。
func (s *Server) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.newUserPipelines().DisableUser(r.Context(), id); err != nil {
		s.Logger.Error("user disable pipeline", "err", err, "user_id", id)
		http.Error(w, "停用失败（已自动回滚 DB 状态）", http.StatusInternalServerError)
		return
	}
	if sess, _ := SessionFrom(r.Context()); sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: "已停用"})
	}
	s.writeAudit(r, "user.disable", fmt.Sprintf("user_id=%d", id))
	http.Redirect(w, r, fmt.Sprintf("/users/%d", id), http.StatusFound)
}

// handleUserDelete POST /users/{id}/delete
//
// v2.86-PR13.0:走 pipeline,RemoveUserConfAndReload 失败时用 store.RecoverUser 原 ID 重建(保留流量)。
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

	if err := s.newUserPipelines().DeleteUser(r.Context(), id); err != nil {
		s.Logger.Error("user delete pipeline", "err", err, "user_id", id, "username", u.Username)
		http.Error(w, "删除失败", http.StatusInternalServerError)
		return
	}

	if sess, _ := SessionFrom(r.Context()); sess != nil && s.FlashStore != nil {
		s.FlashStore.Set(sess.ID, Flash{Message: fmt.Sprintf("用户 %s 已删除", u.Username)})
	}
	s.writeAudit(r, "user.delete", fmt.Sprintf("user_id=%d,username=%s", id, u.Username))
	http.Redirect(w, r, "/users", http.StatusFound)
}

// handleUserDeleteConfirmPage GET /users/{id}/delete
//
// v2.85-PR8(U10):双确认 - 第一步 GET 渲染确认页,不触发实际删除。
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
	s.RenderPage(w, r, http.StatusOK, pageDeleteConfirm, userDeleteConfirmData{
		PageMeta: PageMeta{Page: pageDeleteConfirm, Title: "删除确认", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		User:     u,
	})
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// usernameFromID 用于 disable 时拿 username（terminate 用）。
//
// v2.86-PR13.0:disable 已走 pipeline(内部直接 GetUserByID + Terminate),
// 本函数保留仅为兼容可能的旧引用。
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
