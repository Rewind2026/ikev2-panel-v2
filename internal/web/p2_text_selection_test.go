package web

// 文本选择策略的回归防线(2026-10-13)。
//
// 问题:管理后台双击 / 拖拽会误选一片界面文字,选中块高亮后要再点一次才能
// 取消,和 GitHub / GitLab / Vercel 后台的观感不符。
//
// 修法:body 默认 user-select:none(装饰禁选),再用白名单把**必须能复制**
// 的数据区放开。选「默认禁 + 白名单」而非「逐个禁用」,是因为后者漏一处
// 就失效,且新加组件默认仍可被误选。
//
// 这个测试锁两件事:
//  1. body 级 user-select:none 存在(装饰禁选的基础)。
//  2. 白名单一个都不能少 —— 少任何一条都会造成**功能受损**而非观感问题:
//     少了 code/pre → 服务器地址、命令、证书参数复制不出来;
//     少了 input/textarea → 用户没法选中编辑密码和 DNS 列表;
//     少了 .banner-body → 报错详情复制不了,没法搜解决方案;
//     少了 .one-time-secret → 初始密码记录不下来,而页面明确提示"仅显示这一次"。

import (
	"regexp"
	"strings"
	"testing"
)

const selCSSPath = "../../web/static/style.css"

// selStripComments 剥掉 CSS 注释,避免本文件解释历史用的注释撞上断言。
func selStripComments(src string) string {
	return regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
}

// selRule 找出「选择器组里含 target」的规则块,返回该组的完整选择器文本
// 与 user-select 的值(不含 -webkit- 前缀的那个);找不到返回 ("", "")。
//
// 为什么不能直接用 "target{" 匹配:本文件的规则写成逗号分组
// (summary,.page-header,.page-sub{...}),`summary\s*\{` 匹配不到。
// 必须先取出整组选择器文本,再按逗号切分比对。
//
// 遍历**全部**匹配块而不是第一个 —— body 在 style.css 里出现多次
// (基础样式 + 选择策略),只看第一个块会漏掉后面的声明。
func selRule(css, target string) (string, string) {
	re := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)

	for _, m := range re.FindAllStringSubmatch(css, -1) {
		group := strings.TrimSpace(m[1])
		hit := false
		for _, part := range strings.Split(group, ",") {
			if strings.TrimSpace(part) == target {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		for _, decl := range strings.Split(m[2], ";") {
			d := strings.TrimSpace(decl)
			if !strings.HasPrefix(d, "user-select:") {
				continue // 跳过 -webkit-user-select:xxx
			}
			return group,
				strings.TrimSpace(strings.TrimPrefix(d, "user-select:"))
		}
	}
	return "", ""
}

// selDecl 是 selRule 只取值的便捷包装。
func selDecl(css, selector string) string {
	_, v := selRule(css, selector)
	return v
}

// ---------- 装饰禁选 ----------

// TestSel_BodyDisablesSelection 装饰禁选的基础规则必须存在。
func TestSel_BodyDisablesSelection(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	if got := selDecl(css, "body"); got != "none" {
		t.Errorf("body 应有 user-select:none(装饰禁选的基础),实际 %q", got)
	}
}

// TestSel_DecorSelectorsDisabled 关键装饰区域显式禁选。
//
// 这些不依赖 body 继承:summary 的 UA 默认样式允许选择,page-header 之类
// 是整块文字最容易被误拖到的区域,显式声明才不会因将来给祖先加规则而失效。
func TestSel_DecorSelectorsDisabled(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	for _, sel := range []string{
		"summary",
		".page-header", ".page-sub",
		".empty-title", ".empty-hint",
		".form-help",
	} {
		if got := selDecl(css, sel); got != "none" {
			t.Errorf("%s 应显式 user-select:none,实际 %q", sel, got)
		}
	}
}

// TestSel_SettingsCardSummaryDisabled 折叠面板标题禁选。
//
// 这条规则早于本次改动就存在(user-select:none),但当时漏了
// -webkit-user-select 前缀 —— Safari / iOS 上整条选择策略会静默失效。
func TestSel_SettingsCardSummaryDisabled(t *testing.T) {
	css := p1Read(t, selCSSPath)

	block := regexp.MustCompile(`(?s)\.settings-card > summary\s*\{[^}]*\}`).FindString(css)
	if block == "" {
		t.Fatal("未找到 .settings-card > summary 规则")
	}
	if !strings.Contains(block, "user-select:none") {
		t.Error(".settings-card > summary 应为 user-select:none")
	}
	if !strings.Contains(block, "-webkit-user-select") {
		t.Error(".settings-card > summary 缺少 -webkit- 前缀 —— Safari/iOS 上折叠标题仍可被误选")
	}
}

// ---------- 数据可复制 ----------

// TestSel_DataWhitelistSelectable 数据区必须保持可复制。
//
// 这是本测试最重要的断言:这些选择器缺失会造成功能受损,不是观感问题。
func TestSel_DataWhitelistSelectable(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	group, val := selRule(css, "code")
	if val != "text" {
		t.Fatalf("code 未落在 user-select:text 的白名单规则里(实际 %q)—— "+
			"服务器地址 / 证书参数 / 命令路径都会复制不出来", val)
	}

	required := []struct {
		sel string
		why string
	}{
		{"code", "服务器地址 / 证书参数 / 命令路径都在 <code> 里"},
		{"pre", "Android 与用户详情页的参数块是 <pre><code>"},
		{"input", "用户要选中编辑密码、用户名、DNS 列表"},
		{"textarea", "mc 表单的 dns_servers 是 textarea"},
		{"select", "下拉选项要能选中查看"},
		{"option", "下拉展开后的选项文本要能选中"},
		{".banner-body", "报错详情要复制去搜解决方案 / 提 issue"},
		{".one-time-secret", "初始密码页面提示「仅显示这一次」,必须能复制记录"},
		{".timeline-detail", "审计日志的操作详情要能复制核查"},
		{"dd", "mc 默认值页的 DNS / 间隔等配置值在 <dd> 里"},
	}

	for _, r := range required {
		found := false
		for _, part := range strings.Split(group, ",") {
			if strings.TrimSpace(part) == r.sel {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("白名单缺少 %q —— %s。禁选后无法复制,属功能受损。", r.sel, r.why)
		}
	}
}

// TestSel_SecretClickSelectsAll 一次性密码单击即全选。
//
// 双击只选中一段密码,用户很容易漏掉前后字符 —— user-select:all 让单击全选。
func TestSel_SecretClickSelectsAll(t *testing.T) {
	css := p1Read(t, selCSSPath)

	block := regexp.MustCompile(`(?s)\.one-time-secret code\s*\{[^}]*\}`).FindString(css)
	if block == "" {
		t.Fatal("未找到 .one-time-secret code 规则")
	}
	if !strings.Contains(block, "user-select:all") {
		t.Error(".one-time-secret code 应为 user-select:all(单击全选密码),否则双击会漏字符")
	}
}

// TestSel_NoWebkitPrefixGap 所有 user-select 都应带 -webkit- 前缀。
//
// Safari / iOS Safari 只认 -webkit-user-select,漏了前缀在这些平台上
// 整条策略静默失效。
func TestSel_NoWebkitPrefixGap(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	// 找出所有写了 user-select 但同一声明块内没有 -webkit-user-select 的块
	re := regexp.MustCompile(`(?s)\{([^{}]*)\}`)
	for _, m := range re.FindAllStringSubmatch(css, -1) {
		block := m[1]
		if !strings.Contains(block, "user-select:") {
			continue
		}
		if !strings.Contains(block, "-webkit-user-select") {
			t.Errorf("声明块缺少 -webkit-user-select 前缀(Safari/iOS 会失效):\n%s",
				strings.TrimSpace(block))
		}
	}
}
