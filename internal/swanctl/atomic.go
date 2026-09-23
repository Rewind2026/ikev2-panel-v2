// v2.86-PR15:thin wrapper,把 AtomicWriteFile 转调 internal/fsutil 公共包。
//
// 历史:swanctl.AtomicWriteFile 早期写在这里(2026 早期 PR 反复出现
// "写一半文件半截"的 P0 修复),2026-09 v2.86-PR15 抽到 fsutil 公共包,
// 让 auth.RateLimiter / ddns / 其他持久化场景也能复用同一份原子写逻辑。
//
// 保留 swanctl.AtomicWriteFile 是为了不破坏现有调用方:
//   - internal/swanctl/writer.go
//   - internal/swanctl/loader.go(经 main.go installCertsToSwanctl)
//   - internal/ddns/statefile.go
//   - cmd/ikev2-panel/main.go
//   - 所有测试(*_test.go 大量引用)
//
// 新代码应该直接调 fsutil.AtomicWriteFile;swanctl.AtomicWriteFile
// 仅作为向后兼容存在,未来不再添加新调用方。
package swanctl

import (
	"os"

	"github.com/yourname/ikev2-panel-v2/internal/fsutil"
)

// AtomicWriteFile swanctl 场景特化的原子写(thin wrapper 转调 fsutil.AtomicWriteFile)。
//
// v2.86-PR15 抽到 fsutil 公共包后保留本符号,仅作向后兼容。
// 新代码请直接调 fsutil.AtomicWriteFile。
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return fsutil.AtomicWriteFile(path, data, perm)
}

// _ 抑制 unused import 编译错误(os 在 path 中通过 fsutil.AtomicWriteFile 间接使用,
// 但本文件不再直接用 — 这里只是保险)。
var _ = os.FileMode(0)