package oidc

// metrics_label_coverage_test.go — 每个实际用到的结果标签都必须被预热。
//
// 预热的意义:未出现过的标签在 /metrics 上根本不存在,于是运维**没法预先**为它
// 写告警或看板 —— 想监控"凭据被就地拒绝"的次数,得先等它发生一次。
//
// 手写一张预热清单必然漂:第 8、9 两轮各加了几个标签,一个都没进清单。所以这里
// 扫源码里的 WithLabelValues 字面量,漏一个就是一次 CI 失败并点名 —— 与
// own_credential_coverage_test.go 同一个手法(那个当场抓到了 app_)。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 只匹配字面量参数。变量参数(fl.result 那类经共享尾部下发的)由下面的
// sharedTailLabels 显式列出 —— 它们不在本包的字面量里。
var labelCallRe = regexp.MustCompile(
	`metric(Exchange|BearerExchange)Result\.WithLabelValues\("([a-z_]+)"\)`)

// sharedTailLabels 由 completeExchange 通过 fl.result 下发,两个 flavour 共用。
// 它们不以字面量形式出现在调用点,所以这里显式声明。
var sharedTailLabels = []string{
	"ok", "resolve_fail", "issue_fail", "identity_insert_fail", "race_recovered",
}

func TestMetrics_EveryUsedResultLabelIsPreWarmed(t *testing.T) {
	warm := map[string]bool{}
	for _, l := range exchangeResultLabels() {
		warm["exchange/"+l] = true
	}
	for _, l := range exchangeJWTResultLabels() {
		warm["exchange_jwt/"+l] = true
	}

	used := map[string]string{} // "flavour/label" -> 文件
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		for _, m := range labelCallRe.FindAllStringSubmatch(string(src), -1) {
			flavour := "exchange"
			if m[1] == "BearerExchange" {
				flavour = "exchange_jwt"
			}
			used[flavour+"/"+m[2]] = name
		}
	}
	// 共享尾部的标签两个 flavour 都会用到。
	for _, l := range sharedTailLabels {
		used["exchange/"+l] = "exchange_complete.go"
		used["exchange_jwt/"+l] = "exchange_complete.go"
	}

	if len(used) < 10 {
		t.Fatalf("only %d label uses discovered (%v); the scan is broken and this guard "+
			"would pass vacuously", len(used), used)
	}

	for key, file := range used {
		if !warm[key] {
			parts := strings.SplitN(key, "/", 2)
			t.Errorf("%s emits result label %q (flavour %s) but it is not pre-warmed. "+
				"Until it first occurs the series is absent from /metrics, so no alert or "+
				"dashboard can be written for it in advance. Add it to %sResultLabels()",
				file, parts[1], parts[0],
				map[string]string{"exchange": "exchange", "exchange_jwt": "exchangeJWT"}[parts[0]])
		}
	}
}
