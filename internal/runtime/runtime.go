// 运行时配置合并：env(来自 internal/config) + panelstate 文件(凭证/证书配置)。
//
// 设计动机(审查报告 C3):
//   - v2 之前 internal/config.Load() 直接 import internal/panelstate,
//     跨层反向依赖,启动期 cert.conf / aliyun.creds 各被读两遍。
//   - 现在:internal/config 只读 env,本包负责 env + panelstate 合并,
//     config → runtime 单向依赖,panelstate 完全不知道 config 存在。
//
// 不引入新功能,不改外部行为,仅重构模块边界。
package runtime

import (
	"fmt"
	"os"

	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// Runtime 是启动期把 env + panelstate 合并后的最终配置。
//
// 通过 embed *config.Config 直接拿到所有 env 默认值,只在 panelstate 命中时
// 覆盖 cert.* / aliyun.* 字段。Source 字段独立保存(不在 config 里有)。
type Runtime struct {
	*config.Config

	// Source 字段:启动日志 / 面板显示用,告诉用户这个值从哪儿来。
	AliyunAccessKeyID     string
	AliyunAccessKeySecret string
	AliyunAccessKeySource string // "panelstate" / "env-new" / "env-legacy-ddns" / "env-legacy-acme.sh" / ""
	CertConfigSource      string // "panelstate" / "env-default" / ""
}

// Merge 合并 env 配置和 panelstate 文件,返回最终 Runtime。
//
// 合并规则(高 → 低优先级):
//   1. panelstate 文件(/data/panel-state/cert.conf + aliyun.creds)
//   2. env(IKEV2_*)
//
// 跟 v2 之前 config.Load() 的合并策略保持完全一致(审查报告要求不改外部行为)。
// 唯一区别:现在 panelstate 读盘只发生一次,不在 config.Load 内部重复实例化。
func Merge(env *config.Config) (*Runtime, error) {
	if env == nil {
		return nil, nil
	}
	r := &Runtime{Config: env}

	// 1. 阿里云凭证(panelstate → env-new → env-legacy-ddns → env-legacy-acme.sh)
	// 复用 panelstate.Store,避免重复打开 /data/panel-state/aliyun.creds。
	credStore := panelstate.NewStore()
	if c, err := credStore.LoadAliyun(); err == nil && c != nil {
		r.AliyunAccessKeyID = c.KeyID
		r.AliyunAccessKeySecret = c.KeySecret
		r.AliyunAccessKeySource = "panelstate"
	} else if err != nil {
		// 文件存在但解析失败 → WARN + fallback(跟 v2 config.go 策略一致)
		stderrWarn("panelstate aliyun.creds parse failed", err)
		// env fallback 由 config.Config 内部已处理(LoadAliyunCreds 仍在 env 优先级链)
		r.AliyunAccessKeyID = env.AliyunAccessKeyID
		r.AliyunAccessKeySecret = env.AliyunAccessKeySecret
		r.AliyunAccessKeySource = env.AliyunAccessKeySource
	}
	// panelstate 文件不存在时,config.Load 已经把 env 值写到 Config 上,无需再做

	// 2. 证书配置(panelstate → env)
	certStore := panelstate.NewCertConfigStore()
	if c, err := certStore.LoadCertConfig(); err == nil && c != nil {
		r.CertMode = pickStr(c.CertMode, env.CertMode)
		r.Domain = pickStr(c.Domain, env.Domain)
		r.ServerCN = pickStr(c.ServerCN, env.ServerCN)
		r.ACMEEmail = pickStr(c.ACMEEmail, env.ACMEEmail)
		r.CertConfigSource = "panelstate"
	} else if err != nil {
		stderrWarn("panelstate cert.conf parse failed", err)
		// env fallback
		r.CertMode = env.CertMode
		r.Domain = env.Domain
		r.ServerCN = env.ServerCN
		r.ACMEEmail = env.ACMEEmail
		r.CertConfigSource = env.CertConfigSource
	}

	return r, nil
}

// pickStr a 不为空用 a,否则用 b。
//
// 替换 v2 config.go 里的 orDefault 函数(放这里更符合"合并"语义)。
func pickStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stderrWarn 把启动期 warning 打到 stderr。
//
// 为什么不用 slog:这个函数在 logger 构造之前调用(Logger 依赖 cfg,
// 而 cfg 还在 Load 阶段)。slog.SetDefault 也在 cfg 之后,所以这里只能走 stderr。
func stderrWarn(msg string, err error) {
	fmt.Fprintf(os.Stderr, "WARN: %s: %v\n", msg, err)
}