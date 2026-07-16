package cardmsg_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// expandButNeverCardAllowlist 列出「调用 ExpandAisToBotUIDs 但该路径永不承载
// type-17」的文件（后缀匹配）。唯一成员是用户 send 路径：它在展开之前已按
// Decision 2a 拒绝 type-17，故展开处的 payload 绝不可能是卡片，出站 size 复检
// 无意义。**新增条目必须写明理由**——这是一处显式、极小且结构稳定的豁免，
// 不是「逐个 producer 补齐」的反应式枚举。
var expandButNeverCardAllowlist = map[string]string{
	"modules/message/api.go": "用户 send 路径在展开前已拒 type-17(Decision 2a)，" +
		"展开处 payload 绝非卡片，无出站卡片可复检",
}

// TestType17ProducerSizeRecheckCompleteness 是「type-17 出站 512KiB 复检必须跟在
// 最后一次 size-increasing mutation(mention 展开)之后」不变量的**源码级 guard**
// （PR#543 round-5 blocker 的治本机制）。
//
// 背景：512KiB 上限（Decision 3a）必须作用在真实出站字节，而
// mentionrewrite.ExpandAisToBotUIDs 是 cardmsg.Finalize 之后唯一会增大 payload 的
// mutation（@所有 AI 展开）。此前 size 复检被逐个 producer 手工补齐
// （bot_api → robot → 漏了 incomingwebhook），正是"补对称路径靠人记得"的反应式
// 模式。本测试把它变成机制。
//
// **不变量（文件级）**：任何调用 `ExpandAisToBotUIDs(` 的非测试文件，必须在**同
// 一文件**调用 `cardmsg.RecheckPayloadSize(`——除非它在 expandButNeverCardAllowlist
// 里（展开但上游已拒 type-17）。文件级而非包级：避免「同包别处恰好有一次复检」
// 掩盖「展开后这一处漏了复检」。
//
// 加第四个 type-17 producer（或把某处复检删了）而漏在展开后复检时，本测试直接
// 红，无需等 reviewer 逐个点名。
func TestType17ProducerSizeRecheckCompleteness(t *testing.T) {
	root := repoRoot(t)
	modulesDir := filepath.Join(root, "modules")

	var expandFiles, checkedOrExempt int
	err := filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		// 匹配调用形（带 `(`）而非注释里的裸标识符，压低误报。
		if !strings.Contains(src, "ExpandAisToBotUIDs(") {
			return nil
		}
		expandFiles++
		rel := relPath(root, path)
		if strings.Contains(src, "cardmsg.RecheckPayloadSize(") {
			checkedOrExempt++
			return nil
		}
		if _, ok := expandButNeverCardAllowlist[rel]; ok {
			checkedOrExempt++
			return nil
		}
		t.Errorf("文件 %s 调用 ExpandAisToBotUIDs(mention 展开会增大出站 payload)但未在同文件"+
			"调用 cardmsg.RecheckPayloadSize —— 若该路径承载 type-17，出站 512KiB 复检缺失"+
			"(Decision 3a，三方 producer 对称)。在展开后补 cardmsg.RecheckPayloadSize；"+
			"若该路径结构上永不承载 type-17，加进 expandButNeverCardAllowlist 并写明理由。", rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk modules: %v", err)
	}

	// 兜底：确认扫到了预期数量的展开点（bot_api/robot/incomingwebhook/message ≥4）。
	// 若骤减，说明匹配串或目录结构变了，guard 可能已失效而非"恰好没有 producer"。
	if expandFiles < 4 {
		t.Fatalf("只扫到 %d 个 ExpandAisToBotUIDs 调用文件，期望 ≥4；匹配串或目录结构"+
			"可能已变，guard 失效", expandFiles)
	}
	if checkedOrExempt != expandFiles {
		t.Fatalf("扫到 %d 个展开文件，仅 %d 个已复检或豁免", expandFiles, checkedOrExempt)
	}
}

// repoRoot 从测试 CWD(包目录)向上找含 go.mod 的仓库根。
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
			t.Fatal("未找到 go.mod(仓库根)")
		}
		dir = parent
	}
}

// relPath 返回以 `/` 分隔的仓库相对路径(跨平台稳定,便于 allowlist 后缀匹配)。
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
