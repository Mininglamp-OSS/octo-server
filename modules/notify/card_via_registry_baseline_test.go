package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestBuildDocsAccessRequestCard_MigrationBaseline 是 A11 的"迁移前后自输出字节等价"锁:
//  1. legacy `BuildDocsAccessRequestCard` 输出作为 pre-migration baseline;
//  2. Registry.Render 输出抽出 payload["card"] 后,**删除 metadata.octo.protocol 与
//     metadata.octo.template** 两个新增字段;
//  3. 用 canonical (sorted-key) JSON 序列化两侧,断言字节完全相等。
//
// 相较早期版本 (structural DeepEqual + 只删 template),本版:
//   - 补删 protocol (§5 强制注入的第二个新 field);
//   - 用 canonical JSON 字节相等替代 DeepEqual,任何新增/顺序/别名漂移都会失败;
//   - legacy 的 Action.OpenUrl 已回退到无 id,pilot 的 view_document id 只在
//     Registry.Render 分支加 (见 pkg/cardtmpl/docs_access_request/template.go),
//     所以两侧 actions 也字节相等。
func TestBuildDocsAccessRequestCard_MigrationBaseline(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")
	card := validAccessRequestDocsCard()

	// pre-migration baseline (legacy path)
	legacyRaw, err := n.buildDocsAccessRequestCard(context.Background(), card, "zh-CN")
	if err != nil {
		t.Fatalf("legacy build: %v", err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}

	// post-migration new path
	newRaw, _, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "space-1", card, "zh-CN")
	if err != nil {
		t.Fatalf("new build: %v", err)
	}
	var newer map[string]any
	if err := json.Unmarshal(newRaw, &newer); err != nil {
		t.Fatalf("unmarshal new: %v", err)
	}

	// Strip the two newly injected metadata.octo subfields from the new payload.
	stripped := stripOctoNewFields(newer)

	legacyCanon := canonicalJSON(t, legacy)
	newCanon := canonicalJSON(t, stripped)
	if !bytes.Equal(legacyCanon, newCanon) {
		t.Fatalf("A11 migration baseline drift.\n--- legacy ---\n%s\n--- new (stripped) ---\n%s",
			legacyCanon, newCanon)
	}

	// AC version unchanged
	if v, _ := newer["version"].(string); v != cardmsg.CardVersion {
		t.Errorf("new card.version = %q, want %q", v, cardmsg.CardVersion)
	}
}

// stripOctoNewFields 复制 card map 后删除本 PR 引入的、明确"仅新链路才有"的字段:
//   - metadata.octo.protocol (§5 强制注入)
//   - metadata.octo.template (§5 强制注入)
//   - actions[Action.OpenUrl].id == "view_document"  (pilot Template 按
//     reports/pending.interaction.json 加,legacy 无 id;两侧行为分叉的唯一 action-level 差异)
//
// 返回新 map,不修改入参。这三个删除项是 A11 "迁移前后自输出字节等价" 允许的
// 白名单差异 —— 任何**其它**新增/顺序/别名漂移都会失败。
func stripOctoNewFields(card map[string]any) map[string]any {
	out := deepCloneMap(card)
	if md, _ := out["metadata"].(map[string]any); md != nil {
		if octo, _ := md["octo"].(map[string]any); octo != nil {
			delete(octo, "protocol")
			delete(octo, "template")
			if len(octo) == 0 {
				delete(md, "octo")
			}
		}
	}
	if actions, _ := out["actions"].([]any); actions != nil {
		for _, a := range actions {
			am, _ := a.(map[string]any)
			if am == nil || am["type"] != "Action.OpenUrl" {
				continue
			}
			if id, _ := am["id"].(string); id == "view_document" {
				delete(am, "id")
			}
		}
	}
	return out
}

// canonicalJSON 用递归 sorted-key marshal 保证字节序稳定 (Go 的 json.Marshal 对
// map 已排序 keys,但嵌套 any 里可能是 map/slice,需要遍历确保一致)。
func canonicalJSON(t *testing.T, v any) []byte {
	t.Helper()
	// Go stdlib json.Marshal 对 map[string]any 会按 key 排序;数组顺序保留 caller
	// 意图。这里再走一遍 marshal→unmarshal→marshal 保证 nested 结构也 canonicalized。
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonical marshal step 1: %v", err)
	}
	var round any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("canonical unmarshal: %v", err)
	}
	// json.Marshal 对反序列化后的 map[string]any 会按 key 排序,再序列化即得 canonical。
	final, err := json.Marshal(canonicalize(round))
	if err != nil {
		t.Fatalf("canonical marshal step 2: %v", err)
	}
	return final
}

// canonicalize 递归把 map[string]any 里的 map 转为按 key 有序遍历的形式;
// Go json.Marshal 已按 key 排序 map,所以这里主要是保证 slice 内的元素也走一遍。
func canonicalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[k] = canonicalize(x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonicalize(e)
		}
		return out
	default:
		return x
	}
}

// deepCloneMap 通过 marshal/unmarshal 深拷贝 map,防止 stripOctoNewFields 影响原值。
func deepCloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return m
	}
	return out
}
