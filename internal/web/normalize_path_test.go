// v2.86-PR19 (A14) 回归测试：路径归一化防 label 基数爆炸。
//
// 背景：GET /install/{token} 是公开无认证路由，token 是 base64url(32B)=43 字符。
// 原 normalizePath 只归一化纯数字段，token 含数字 → 判定失败 → 每个 token
// 生成一条全新 metric label 组合。匿名攻击者反复请求随机路径即可让
// httpRequests map 无界增长（内存耗尽 DoS），同时 token 明文进 /metrics。

package web

import "testing"

func TestNormalizePath_UserID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/users/123", "/users/{id}"},
		{"/users/1/reset-password", "/users/{id}/reset-password"},
		{"/api/topology", "/api/topology"},
		{"/", "/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizePath(c.in); got != c.want {
			t.Errorf("normalizePath(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestNormalizePath_InstallToken 是 A14 的核心断言。
func TestNormalizePath_InstallToken(t *testing.T) {
	// installtoken 生成的真实形态：base64url(32B) = 43 字符
	realToken := "aB3dEfGh1jKlMnOpQrStUvWxYz0123456789-_ABCDE"
	if len(realToken) != 43 {
		t.Fatalf("测试样本长度应为 43，实际 %d", len(realToken))
	}

	got := normalizePath("/install/" + realToken)
	if got != "/install/{token}" {
		t.Errorf("normalizePath(/install/<43char>) = %q, 期望 /install/{token}", got)
	}
}

// TestNormalizePath_TokenCardinalityBounded 验证任意随机 token 都被归一化，
// 因此不会产生无限多的 label 组合。
func TestNormalizePath_TokenCardinalityBounded(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		// 模拟不同随机 token。"tok"(3) + 3 个变体字符 + 37 个 'x' = 43,
		// 与 installtoken.Generate 的 base64url(32B) 等长。
		tok := "tok" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) +
			string(rune('a'+(i/676)%26)) + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
		if len(tok) != 43 {
			t.Fatalf("token 样本长度应为 43，实际 %d", len(tok))
		}
		p := normalizePath("/install/" + tok)
		seen[p] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("1000 个不同 token 产生了 %d 个 label 组合，期望 1 个（内存耗尽 DoS）", len(seen))
	}
}

func TestIsInstallToken(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"aB3dEfGh1jKlMnOpQrStUvWxYz0123456789-_ABCDE", true}, // 43 字符真实形态
		{"123", false},   // 太短
		{"users", false}, // 太短
		{"abcdefghij0123456789abcdefghij0123456789", true}, // 40 字符
		{"short", false},
		{"abcdefghij0123456789abcdefghij012345678/aa", false}, // 含非法字符 '/'
		{"abcdefghij0123456789abcdefghij012345678+aa", false}, // base64url 不含 '+'
		{"abcdefghij0123456789abcdefghij012345678=aa", false}, // base64url 不含 '='
	}
	for _, c := range cases {
		if got := isInstallToken(c.in); got != c.want {
			t.Errorf("isInstallToken(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

// TestIsInstallToken_RejectsSlash 确保含 '/' 的段不会被误判为 token
// （否则普通路径段会被错误归一化）。
func TestIsInstallToken_RejectsSlash(t *testing.T) {
	if isInstallToken("abcdefghij0123456789abcdefghij0123456/") {
		t.Error("含 '/' 的段不应被判为 install token")
	}
}
