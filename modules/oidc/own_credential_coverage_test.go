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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// credentialPrefixDeclRe 匹配形如 `XxxTokenPrefix = "app_"` 的常量声明。
var credentialPrefixDeclRe = regexp.MustCompile(
	`(?m)^\s*([A-Z][A-Za-z0-9]*(?:Token|APIKey|Key)Prefix)\s*=\s*"([^"]+)"`)

// credentialMintingPackages 会签发 bearer 凭据的模块。
//
// 新增这类模块时要加进来 —— 而"忘了加"这件事本身没法自动发现,所以这里
// 顺带钉住一个更弱但可检的性质:下面 mustCoverPrefixes 里的每个前缀都必须
// 仍然存在于源码中(常量被改名/移动时也会红)。
var credentialMintingPackages = []string{
	"../app_bot",
	"../botfather",
}

// nonBearerPrefixes 这些常量名字像凭据前缀,但实际是 Redis key 命名空间,
// 不会出现在 Authorization 头上。列在这里而不是靠正则排除,是为了让每次豁免
// 都有一个人为的判断记录。
var nonBearerPrefixes = map[string]bool{
	"welcomeSentKeyPrefix": true,
}

func TestOwnCredentialDetector_KnowsEveryBearerCredentialPrefix(t *testing.T) {
	d := newDetector(&fakeTokenReader{}) // 会话必定落空,只测前缀分支

	found := map[string]string{} // 常量名 -> 前缀值
	for _, pkg := range credentialMintingPackages {
		entries, err := os.ReadDir(pkg)
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
				strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(pkg, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			for _, m := range credentialPrefixDeclRe.FindAllStringSubmatch(string(src), -1) {
				if nonBearerPrefixes[m[1]] {
					continue
				}
				found[m[1]] = m[2]
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no credential prefix constants were discovered; the scan is broken, " +
			"which would make this guard silently vacuous")
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
