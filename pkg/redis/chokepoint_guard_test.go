package redis

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoRawRedisClientOutsideChokepoint 把本 PR 的核心不变量钉成源码守卫:生产代码
// 只能经 octoredis 的 NewInstrumentedClient / InstrumentedClientFromOptions 构造 redis
// 客户端,不得直接 rd.NewClient / redis.NewClient —— 否则新站点会静默重开
// dependency="redis" 的盲区(COMPREHENSION §2 命名的回归)。仿照 i18n 的
// Test*NoLegacyResponseError 源码守卫。
//
// 扫描范围:仓库根下所有非 _test.go 的 .go 文件,排除 chokepoint 自身所在的 pkg/redis。
func TestNoRawRedisClientOutsideChokepoint(t *testing.T) {
	root := repoRoot(t)
	chokepoint := filepath.Join(root, "pkg", "redis")
	// \b 防止把 octoredis.NewClient(若有)误判成 redis.NewClient。
	re := regexp.MustCompile(`\b(rd|redis)\.NewClient\(`)

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			if path == chokepoint {
				return filepath.SkipDir // chokepoint 自身合法持有 rd.NewClient
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if re.Match(b) {
			rel, _ := filepath.Rel(root, path)
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("raw redis client construction outside the octoredis chokepoint — route through "+
			"octoredis.NewInstrumentedClient / InstrumentedClientFromOptions so commands feed "+
			"dependency=\"redis\":\n  %s", strings.Join(violations, "\n  "))
	}
}

// repoRoot 从测试运行目录向上找到含 go.mod 的仓库根。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}
