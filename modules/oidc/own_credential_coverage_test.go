package oidc

// own_credential_coverage_test.go — 一道源码守卫:本服务新增一种 bearer 凭据前缀
// 而忘了教给 OwnCredentialDetector,这里就红。
//
// 为什么需要这道守卫而不是靠人记住:Classify 的前缀分支本质是一张**"想起来了的
// 类型"的名单**,任何没被枚举的凭据默认走"转发上游"。那是 taxonomy 层面的
// fail-open。同一个改动里已经三次栽在同一件事上 —— 守卫本身没错,是**输入清单**
// 漏了。清单漏项无法靠再读一遍代码防住,只能让它变成一次测试失败。
//
// 这不是编译期强制(那需要把各模块的前缀常量收进一个注册表并取消导出,是一次
// 跨模块重构)。但它把"漏一种凭据"从一次生产泄漏降级成一次 CI 失败,而且指出
// 具体是哪个常量没被覆盖。

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// credentialPrefixDeclRe 匹配形如 `XxxTokenPrefix = "app_"` 的常量声明。
var credentialPrefixDeclRe = regexp.MustCompile(
	`(?m)^\s*([A-Z][A-Za-z0-9]*(?:Token|APIKey|Key)Prefix)\s*=\s*"([^"]+)"`)

// modulesRoot 扫描根 —— **整个 modules/**,不是一张手写的包清单。
//
// 第一版只列了 app_bot 和 botfather 两个包。那样的话"新增一个签发凭据的模块"
// 仍然要靠人记得来改这张清单 —— 也就是把同一个"名单会漏"的毛病搬了个位置。
// 扫全部模块之后,新模块里的凭据前缀常量会自动进入检查范围。
//
// 全仓验证过这个正则在 modules/ 下只命中三个真前缀(app_/bf_/uk_),没有误报;
// pkg/botevent 里那几个 Redis key 前缀不在扫描范围内。
const modulesRoot = ".."

// nonBearerPrefixes 豁免表:名字匹配但实际不会出现在 Authorization 头上的常量
// (例如 Redis key 命名空间)。
//
// **当前为空** —— 目前没有需要豁免的。保留这个机制是为了让将来的豁免必须是一次
// 显式的人为判断,而不是靠改正则悄悄放过。
var nonBearerPrefixes = map[string]bool{}

func TestOwnCredentialDetector_KnowsEveryBearerCredentialPrefix(t *testing.T) {
	d := newDetector(&fakeTokenReader{}) // 会话必定落空,只测前缀分支

	found := map[string]string{} // 常量名 -> 前缀值
	err := filepath.WalkDir(modulesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 本模块自己不签发 bearer 凭据,跳过可以避免把示例串当成声明。
			if d.Name() == "oidc" || d.Name() == "sql" || d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range credentialPrefixDeclRe.FindAllStringSubmatch(string(src), -1) {
			if nonBearerPrefixes[m[1]] {
				continue
			}
			found[m[1]] = m[2]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", modulesRoot, err)
	}

	// 扫描坏掉(路径变了、正则不再匹配)会让这道守卫静默变成永远通过。
	// 钉一个下界:已知至少存在 uk_ / bf_ / app_ 三个。
	if len(found) < 3 {
		t.Fatalf("only %d credential prefix constant(s) discovered (%v); at least three are "+
			"known to exist, so the scan is broken and this guard would pass vacuously",
			len(found), found)
	}

	for name, prefix := range found {
		kind, err := d.Classify(context.Background(), prefix+"0123456789abcdef")
		if err != nil {
			t.Errorf("%s (%q): Classify errored: %v", name, prefix, err)
			continue
		}
		if kind == OwnCredentialNone {
			t.Errorf("%s = %q is a credential this service mints, but "+
				"OwnCredentialDetector does not recognise it. Under kind=oauth2 a token "+
				"with this prefix presented on Authorization: Bearer is written verbatim "+
				"into the upstream /userinfo URL query — a third party's access log. "+
				"Add it to Classify and to the C4 row of guard-matrix.md", name, prefix)
		}
	}
}

// 前缀无关的旁证:一个**没有**任何已知前缀、也不在会话存储里的串必须放行。
// 否则上面那道覆盖检查可以靠"什么都判成我方凭据"作弊通过。
func TestOwnCredentialDetector_CoverageGuardIsNotVacuous(t *testing.T) {
	kind, err := newDetector(&fakeTokenReader{}).
		Classify(context.Background(), "mock-access-token")
	if err != nil || kind != OwnCredentialNone {
		t.Fatalf("an unrelated opaque token was classified as ours (%q, %v); the coverage "+
			"test above would then pass no matter what Classify does", kind, err)
	}
}
