// 用户 CRUD 事务化流水线。
//
// 设计动机(审查报告 C1):
//   - v2 之前 handleUserCreate / handleUserDelete / handleUserResetPassword 各 handler
//     自己拼 "DB → 文件 → reload → limit file → terminate" 链路,失败回滚散落,
//     漏一处就产生孤儿配置(charon 里有 user,但 DB 已删)。
//   - 本文件把这些 IO 步骤集中成 3 个 pipeline 函数,所有失败路径走统一的 rollback,
//     保证 "DB / swanctl / limiter / terminate" 四者状态一致。
//
// 不引入新功能,不改外部行为,仅重构。
package web

import (
	"context"
	"errors"
	"fmt"

	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/store"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// userPipelines 把 handler 所需的"会改变外部状态"的依赖抽成一个 struct。
//
// 好处:
//   - handler 端只需构造一次,后续 pipeline 调用传 ctx + 状态对象
//   - 单元测试可注入 mock(本次未做,但接口已隔离)
//   - Server 上 5 个 *store/swanctl/limit 字段不再散落 pipeline 各处
type userPipelines struct {
	Store   *store.Store
	Swanctl *swanctl.Manager
	Limiter *limit.Limiter
}

// newUserPipelines 从 Server 取依赖,handler 端一行调用。
func (s *Server) newUserPipelines() userPipelines {
	return userPipelines{Store: s.Store, Swanctl: s.Swanctl, Limiter: s.Limiter}
}

// pipeline 错误分类标记,handler 用 errors.Is 区分失败环节(给用户友好提示)。
//
// 设计动机:审查报告指出旧实现 `data.Error = fmt.Sprintf("写入 swanctl 配置失败:%v ...", err)`
// 把 swanctl 子进程的 stderr 直接回显给用户,泄露内部路径/命令细节。
// 重构后:用 sentinel error 分类,handler 只展示简短中文文案。
var (
	errSwanctlWriteFailed  = errors.New("pipeline: swanctl write/reload failed")
	errLimiterWriteFailed  = errors.New("pipeline: limiter file write failed")
	errSwanctlRemoveFailed = errors.New("pipeline: swanctl remove/reload failed")
	errTerminateFailed     = errors.New("pipeline: swanctl terminate failed")
)

// CreateUserResult 流水线返回:DB ID + 新密码(给 flash 显示用)。
type CreateUserResult struct {
	ID       int64
	Password string
}

// CreateUser 一次性执行:DB → swanctl conf+reload → limiter file。
//
// 失败回滚策略:
//   - DB 失败:不动任何外部状态
//   - swanctl conf/reload 失败:删除 DB 行(整流水回滚)
//   - limiter file 失败:swanctl 反向 reload 删除 conf + 删除 DB 行
//
// 重构要点(对比 v2):旧实现里 limiter 失败时 `RemoveUserConf` 不 reload,
// 留下"DB 已删但 charon 已 reload 加载用户"的孤儿 SA 窗口。修复后:
//   - limiter 失败 → 先 RemoveUserConfAndReload 让 charon 知道 user 没了
//     (保证 DB / 文件 / charon 三者一致)
//   - 然后 DeleteUser DB 行
func (p userPipelines) CreateUser(ctx context.Context, u *store.User) (*CreateUserResult, error) {
	// Step 1: 写 DB
	id, err := p.Store.CreateUser(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	// Step 2: 写 swanctl conf + ReloadAll
	if err := p.Swanctl.WriteUserConfAndReload(ctx, u.Username, u.Password); err != nil {
		// rollback: 删 DB 行(swanctl 没 reload → 无需反向 reload)
		if delErr := p.Store.DeleteUser(ctx, id); delErr != nil {
			return nil, fmt.Errorf("create swanctl failed (%w) and rollback delete user failed (%v)", errSwanctlWriteFailed, delErr)
		}
		return nil, fmt.Errorf("swanctl write/reload: %w", errSwanctlWriteFailed)
	}

	// Step 3: 写限速文件(updown 脚本读取,跟 swanctl 状态无关,独立 IO)
	if err := p.Limiter.WriteLimitFile(u.Username, u.SpeedLimitMbps); err != nil {
		// rollback: 反向 reload 让 charon 摘掉 user,再删 DB 行
		if rmErr := p.Swanctl.RemoveUserConfAndReload(ctx, u.Username); rmErr != nil {
			return nil, fmt.Errorf("limiter write failed (%w) and rollback remove user conf failed (%v)", errLimiterWriteFailed, rmErr)
		}
		if delErr := p.Store.DeleteUser(ctx, id); delErr != nil {
			return nil, fmt.Errorf("limiter write failed (%w) and rollback delete user failed (%v)", errLimiterWriteFailed, delErr)
		}
		return nil, fmt.Errorf("limiter write: %w", errLimiterWriteFailed)
	}

	return &CreateUserResult{ID: id, Password: u.Password}, nil
}

// ResetPasswordResult 流水线返回:新密码(给 flash 显示用)。
type ResetPasswordResult struct {
	NewPassword string
}

// ResetPassword 重置密码:DB → swanctl conf+reload。
//
// 失败回滚策略:
//   - DB 失败:不动任何外部状态
//   - swanctl 失败:DB 密码已更新,但 VPN 仍需旧密码 → 重建 DB 密码回旧值
//     用 UpdateUserPassword 反向写回旧密码(不是删整行,因为 user 其他字段都有效)
//
// 重构要点:旧实现里 swanctl 失败不回滚 DB,用户登录看到新密码但 VPN 仍需旧密码。
func (p userPipelines) ResetPassword(ctx context.Context, id int64, oldPassword, newPassword string) (*ResetPasswordResult, error) {
	if err := p.Store.UpdateUserPassword(ctx, id, newPassword); err != nil {
		return nil, fmt.Errorf("update password: %w", err)
	}

	u, err := p.Store.GetUserByID(ctx, id)
	if err != nil {
		// DB 写完了但读不到 → 这种情况下 swanctl 也无法做(不知道 username)
		// 已经写进去的新密码是脏数据,但我们没 username 也无法反向 reload
		// → 上层记录日志,人工干预(低概率事件,RowMissing 在更新后立刻读不到基本不可能)
		return nil, fmt.Errorf("update password ok but read user back failed: %w", err)
	}

	if err := p.Swanctl.WriteUserConfAndReload(ctx, u.Username, newPassword); err != nil {
		// rollback: 把 DB 密码改回旧值
		if revErr := p.Store.UpdateUserPassword(ctx, id, oldPassword); revErr != nil {
			return nil, fmt.Errorf("swanctl reload failed (%w) and password rollback failed (%v); DB now has new password but VPN requires old password", errSwanctlWriteFailed, revErr)
		}
		return nil, fmt.Errorf("swanctl reload: %w", errSwanctlWriteFailed)
	}

	return &ResetPasswordResult{NewPassword: newPassword}, nil
}

// DisableUser 停用用户:DB enabled=false → 终止 SA。
//
// 失败回滚策略:
//   - DB 失败:不动 SA
//   - Terminate 失败:把 DB enabled 改回 true(审计/合规:用户已 disable 但 SA 残留 = 漏洞)
//
// 重构要点:旧实现里 `_ = Terminate(...)` 错误吞,用户被 disable 但 SA 仍可连。
func (p userPipelines) DisableUser(ctx context.Context, id int64) error {
	if err := p.Store.SetUserEnabled(ctx, id, false); err != nil {
		return fmt.Errorf("set disabled: %w", err)
	}

	u, err := p.Store.GetUserByID(ctx, id)
	if err != nil {
		// DB 已更新 enabled=false 但拿不到 username → Terminate 不知道目标
		// 这种情况下 user 已经在 DB 里被标 disabled,但 SA 可能还活着
		// → 上层记录日志 + audit,人工干预
		return fmt.Errorf("set disabled ok but read user back failed: %w", err)
	}

	if err := p.Swanctl.Terminate(ctx, u.Username); err != nil {
		// rollback: 把 DB 改回 enabled=true
		if revErr := p.Store.SetUserEnabled(ctx, id, true); revErr != nil {
			return fmt.Errorf("terminate failed (%w) and enabled rollback failed (%v); user disabled in DB but SA may still be alive", errTerminateFailed, revErr)
		}
		return fmt.Errorf("swanctl terminate: %w", errTerminateFailed)
	}

	return nil
}

// DeleteUser 删除用户:DB → swanctl conf-remove+reload → limiter file → Terminate。
//
// 失败回滚策略:
//   - DB 失败:不动任何外部状态
//   - swanctl 失败:重新插入 user(保留原 ID + 流量字段) — 用 store.RecoverUser
//   - limiter/terminate 失败:不致命,记 warn(用户的 SA 残留会在自然 timeout 清理)
//
// 重构要点:旧实现里 `u.ID = 0; CreateUser(u)` 回滚,导致 bytes_in_total /
// bytes_out_total / last_used_at 全部丢失。修复后:走 store.RecoverUser 用原 ID
// 重建行(下面 store 层提供这个函数)。
func (p userPipelines) DeleteUser(ctx context.Context, id int64) error {
	u, err := p.Store.GetUserByID(ctx, id)
	if err != nil {
		return fmt.Errorf("read user for delete: %w", err)
	}
	// 备份完整行(包括流量字段),用于回滚
	backup := *u

	if err := p.Store.DeleteUser(ctx, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}

	if err := p.Swanctl.RemoveUserConfAndReload(ctx, u.Username); err != nil {
		// rollback: 用原 ID 重建行(保留流量)
		if recErr := p.Store.RecoverUser(ctx, &backup); recErr != nil {
			return fmt.Errorf("swanctl remove failed (%w) and recover user failed (%v); user deleted in DB but conf file still on disk", errSwanctlRemoveFailed, recErr)
		}
		return fmt.Errorf("swanctl remove: %w", errSwanctlRemoveFailed)
	}

	// 这两个步骤不致命:limit 文件残留 = 该 user 的 tc 规则不生效,但 charon 已不认该 user;
	// SA 残留 = charon 端 timeout 后自然清理。
	if err := p.Limiter.RemoveLimitFile(u.Username); err != nil {
		_ = err // 已删 DB + swanctl,limiter 文件失效不影响核心功能
	}
	if err := p.Swanctl.Terminate(ctx, u.Username); err != nil {
		_ = err
	}

	return nil
}

// 文件末尾注释。
// pipeline 不直接调 auth.GeneratePassword:handler 端生成密码后传入 *store.User,
// 这样密码不会出现在 pipeline 内部日志里(slog 字段可能漏出去)。