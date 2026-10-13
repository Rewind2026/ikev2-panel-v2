package web

// 密码 eye 按钮按压错位的回归防线(2026-10-13)。
//
// 现象:登录页点「显示/隐藏密码」的 eye 按钮时,图标整体下坠约 18px 再弹回,
// 观感是"按钮跳出去了",iPhone 上尤其明显。
//
// 根因纯 CSS,不是 JS:
//
//	.password-toggle{ transform:translateY(-50%); }   ← 用 transform 做垂直居中
//	button:active    { transform:translateY(1px);  }   ← 全局按压动效
//
// `button:active` 的特异性 (0,1,1) 高于 `.password-toggle` 的 (0,1,0),
// 于是 :active 一命中,transform 被**整条覆盖**,translateY(-50%) 消失,
// 居中失效 → 图标掉到自身高度一半的位置(34/2 + 1 ≈ 18px)。
// 又因为 .password-toggle 的 transition 只声明了 color/background-color/border-color,
// 不含 transform,所以这个位移没有任何过渡,是瞬时跳变;iOS Safari 上 :active
// 还会粘住一小会儿,跳出去的现象被拉长。
//
// 定下来的规矩:**transform 不能同时承担定位和按压动效。**
// 绝对定位元素的垂直居中一律用 top + bottom + margin-top/bottom:auto。
// 按压反馈允许存在(全局 1px 下沉),但定位不能依赖它。
//
// 参见 docs/TROUBLESHOOTING.md §9.10。

import (
	"regexp"
	"strings"
	"testing"
)

// p3RuleBlocks 返回 CSS 里所有「声明块含 needle」的规则,格式 "选择器组 {声明}"。
// 注释已在调用前剥掉,规则组按逗号切分比对(与 selRule 同样的坑:分组选择器
// 不能用 "target{" 直接匹配)。
func p3RuleBlocks(css, needle string) []string {
	re := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	var out []string

	for _, m := range re.FindAllStringSubmatch(css, -1) {
		group := strings.TrimSpace(m[1])
		if !p3GroupHas(group, needle) {
			continue
		}
		out = append(out, group+" {"+m[2]+"}")
	}
	return out
}

// p3GroupHas 判断逗号分组里是否含某个选择器。
func p3GroupHas(group, target string) bool {
	for _, part := range strings.Split(group, ",") {
		if strings.TrimSpace(part) == target {
			return true
		}
	}
	return false
}

// TestP3_EyeToggleCentersWithoutTransform 直接锁住本次修复。
func TestP3_EyeToggleCentersWithoutTransform(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	blocks := p3RuleBlocks(css, ".password-toggle")
	if len(blocks) == 0 {
		t.Fatal("style.css 里找不到 .password-toggle 规则 —— eye 按钮样式被删了?")
	}
	// :active 是独立规则组,选择器文本是 ".password-toggle:active",不等于
	// ".password-toggle",得单独捞。
	activeBlocks := p3RuleBlocks(css, ".password-toggle:active")

	// 基础规则:选择器组**恰好**是 .password-toggle(不含 :hover / [aria-pressed] /
	// 后代选择器)。变体都带额外成分,组文本不会等于它。
	var base string
	for _, b := range blocks {
		if strings.HasPrefix(b, ".password-toggle {") {
			base = b
			break
		}
	}
	if base == "" {
		t.Fatal("找不到 .password-toggle 的基础规则块")
	}

	if strings.Contains(base, "transform") {
		t.Errorf("`.password-toggle` 基础规则不得声明 transform —— 会被全局 "+
			"`button:active{transform:translateY(1px)}`(特异性更高)整条覆盖,"+
			"垂直居中失效,图标按压时下坠约 18px:\n%s", p1Trunc(base))
	}

	for _, need := range []string{
		"position:absolute",
		"top:0",
		"bottom:0",
		"margin-top:auto",
		"margin-bottom:auto",
	} {
		if !strings.Contains(base, need) {
			t.Errorf("`.password-toggle` 基础规则缺少 %q —— 垂直居中必须靠 "+
				"top+bottom+margin:auto,不能靠 transform(见本文件头注释)", need)
		}
	}

	// 第二层:居中改掉之后,全局 button:active 的 translateY(1px) 仍会生效,
	// 按压时还有 1px 位移。必须显式 transform:none 断掉 —— 这个图标浮在输入框
	// 内部,1px 也看得出来是"错位",而不是按压反馈。
	//
	// 这条曾经漏掉:第一次只改了居中就以为完事,实机仍有轻微跳动。
	var activeRule string
	for _, b := range activeBlocks {
		if strings.HasPrefix(b, ".password-toggle:active {") {
			activeRule = b
			break
		}
	}
	if activeRule == "" {
		t.Fatal("缺少 `.password-toggle:active` 规则 —— 全局 button:active 的 " +
			"translateY(1px) 会让图标按压时仍位移 1px")
	}
	if !strings.Contains(activeRule, "transform:none") {
		t.Errorf("`.password-toggle:active` 必须声明 transform:none 抵消全局按压位移:\n%s",
			p1Trunc(activeRule))
	}
}

// TestP3_NoAbsoluteCenteringViaTransform 全量兜底:任何绝对定位 + top:50% 的规则
// 都不能再用 transform:translateY(-50%) 居中。写下这个坑的是 .password-toggle,
// 同样的写法迟早会被复制到别的浮层按钮上,所以在文件级拦住。
func TestP3_NoAbsoluteCenteringViaTransform(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	for _, b := range p3RuleBlocks(css, "top:50%") {
		if !strings.Contains(b, "position:absolute") {
			continue
		}
		if strings.Contains(b, "translateY(-50%)") {
			t.Errorf("绝对定位元素用 transform:translateY(-50%%) 居中,会被全局 "+
				"`button:active` 覆盖导致按压错位。改用 top+bottom+margin:auto:\n%s",
				p1Trunc(b))
		}
	}
}

// TestP3_PressOffsetStaysSubtle 锁住用户抱怨的"动画太夸张"。
// 全局按压位移是刻意的微反馈,超过 2px 就开始显得在"跳"。
func TestP3_PressOffsetStaysSubtle(t *testing.T) {
	css := selStripComments(p1Read(t, selCSSPath))

	for _, b := range p3RuleBlocks(css, "button:active") {
		// 只认位移值,忽略 background 之类的声明。
		for _, decl := range strings.Split(b[strings.Index(b, "{")+1:], ";") {
			d := strings.TrimSpace(decl)
			if !strings.HasPrefix(d, "transform:translateY(") {
				continue
			}
			v := strings.TrimSuffix(strings.TrimPrefix(d, "transform:translateY("), ")")
			n := 0
			neg := false
			for i, r := range v {
				if i == 0 && r == '-' {
					neg = true
					continue
				}
				if r < '0' || r > '9' {
					n = -1
					break
				}
				n = n*10 + int(r-'0')
			}
			if n < 0 {
				continue // 非纯数字(calc/var)不在本测试范围
			}
			if neg {
				n = -n
			}
			if n > 2 || n < -2 {
				t.Errorf("全局 button:active 位移 %dpx 过大会显得在「跳」,控制在 ±2px:\n%s",
					n, p1Trunc(b))
			}
		}
	}
}
