package oidc

// metrics_label_coverage_test.go — 每个实际用到的结果标签都必须被预热。
//
// 预热的意义:未出现过的标签在 /metrics 上根本不存在,于是运维**没法预先**为它写
// 告警或看板 —— 想监控"凭据被就地拒绝"的次数,得先等它发生一次。
//
// 手写清单必然漂:第 8、9 两轮各加了标签,一个都没进清单。所以这里扫源码里的
// WithLabelValues 字面量,漏一个就是一次 CI 失败并点名。
//
// 守卫自身的完整性也要自检 —— 第一版只覆盖了 8 个 metric 里的 2 个,而其余 6 个
// 用的是同一种会漂的手写清单。所以下面先断言 **metrics.go 里声明的每一个 Vec 都在
// 映射表里**,再逐个查标签;映射表漏一个 metric 同样是失败。
//
// **已知局限,明说**:标签由变量传入的调用点(如 metricCallbackTotal.WithLabelValues(result))
// 静态扫不出来,本守卫覆盖不到。它们仍靠人工核对,别把这里的绿当成全覆盖。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prewarmed 每个 metric 变量名 → 它被预热的标签集合。
//
// 多标签 metric(bind 的 endpoint × result)只登记"存在",不做值检查 ——
// 值的组合由预热块自己的双层循环负责,单标签字面量扫描表达不了。
func prewarmed() map[string]map[string]bool {
	single := map[string][]string{
		"metricCallbackTotal":               callbackResultLabels(),
		"metricStateConsumeTotal":           stateConsumeResultLabels(),
		"metricLogoutTotal":                 logoutResultLabels(),
		"metricSyncTickTotal":               syncTickResultLabels(),
		"metricSyncProcessedTotal":          syncProcessedResultLabels(),
		"metricSyncVerificationSyncedTotal": syncVerificationSyncedResultLabels(),
		"metricExchangeResult":              exchangeResultLabels(),
		"metricBearerExchangeResult":        exchangeJWTResultLabels(),
		"metricInitialSpaceJoinTotal":       initialSpaceJoinResultLabels(),
	}
	out := map[string]map[string]bool{}
	for m, labels := range single {
		set := map[string]bool{}
		for _, l := range labels {
			set[l] = true
		}
		out[m] = set
	}
	// 多标签:登记为已知,不做值检查。
	out["metricBindRequestTotal"] = nil
	out["metricBindRequestDuration"] = nil
	return out
}

var vecDeclRe = regexp.MustCompile(`(metric[A-Za-z0-9]+)\s*=\s*promauto\.New[A-Za-z]*Vec\(`)

// 映射表必须覆盖 metrics.go 声明的每一个 *Vec。漏一个 metric,它的标签就完全没人管。
func TestMetrics_EveryVecIsAccountedForByTheCoverageGuard(t *testing.T) {
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	declared := vecDeclRe.FindAllStringSubmatch(string(src), -1)
	if len(declared) < 8 {
		t.Fatalf("only %d metric Vec declarations found; the scan is broken", len(declared))
	}
	known := prewarmed()
	for _, m := range declared {
		if _, ok := known[m[1]]; !ok {
			t.Errorf("%s is a label-carrying metric but the coverage guard does not know "+
				"about it, so nothing checks that its emitted labels are pre-warmed. Add it "+
				"to prewarmed()", m[1])
		}
	}
}

// labelCallRe 匹配"具名 metric 上的字面量标签",以及 completeExchange 经 fl.result
// 下发的字面量(那条尾部两个 flavour 共用,所以两边都要预热)。
var labelCallRe = regexp.MustCompile(
	`(?:(metric[A-Za-z0-9]+)|(fl)\.result)\.WithLabelValues\("([a-z_]+)"\)`)

func TestMetrics_EveryLiteralResultLabelIsPreWarmed(t *testing.T) {
	known := prewarmed()

	type use struct{ metric, label, file string }
	var uses []use

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
			switch {
			case m[2] == "fl":
				// 共享尾部:两个 exchange flavour 都会走到。
				uses = append(uses,
					use{"metricExchangeResult", m[3], name},
					use{"metricBearerExchangeResult", m[3], name})
			default:
				uses = append(uses, use{m[1], m[3], name})
			}
		}
	}

	// 空转下界只约束**扫描本身**的产出。第一版把手写的共享尾部清单也算进来再比,
	// 而那张清单正好凑满下界,于是扫描完全失效也照样通过。
	if len(uses) < 5 {
		t.Fatalf("only %d literal label use(s) found by the source scan; the scan is broken "+
			"(regex, working directory, or file naming) and this guard would pass vacuously",
			len(uses))
	}

	for _, u := range uses {
		set, ok := known[u.metric]
		if !ok {
			t.Errorf("%s emits %s.WithLabelValues(%q) but that metric is not in prewarmed()",
				u.file, u.metric, u.label)
			continue
		}
		if set == nil {
			continue // 多标签 metric,不做值检查
		}
		if !set[u.label] {
			t.Errorf("%s emits result label %q on %s but it is not pre-warmed. Until it "+
				"first occurs the series is absent from /metrics, so no alert or dashboard "+
				"can be written for it in advance", u.file, u.label, u.metric)
		}
	}
}
