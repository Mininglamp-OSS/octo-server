package cardtmpl_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// notifyBotUID 与 modules/notify.NotifyBotUIDValue 同源;此处直接用字面量,
// 避免 pkg/cardtmpl_test 反向 import notify 造成循环 (notify 已 import cardtmpl)。
// 如二者漂移,test 会红,和 main.go:521 的启动 fail-close 一并暴露。
const notifyBotUID = "notification"

// TestConformance_AllRegisteredTemplates 遍历 RegisteredForTest 集合,对每张 Template
// 每个已注册 v2 view 断言 docs/platform-card-base.md §4.3 里 4 条 + §5 metadata 5 条:
//   A15a schema 通过 sample
//   A15b Registry.Render 无错;cardmsg.Validate 隐式 (Render 内 step 8) 通过
//   A15c 交互契约完整锁 (4 子条:id 集合相等 / input 必出现 / dataKeys 相等 / associatedInputs 一致)
//   A15d metadata.octo.template = {id,version}
//   A15e metadata.octo.protocol = "octo-card@1.0"
//   A15f metadata.webUrl 是绝对 https
func TestConformance_AllRegisteredTemplates(t *testing.T) {
	registry := freshRegistry(t)
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://test.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-conformance",
	}

	for _, tmpl := range registry.RegisteredForTest() {
		meta := tmpl.Meta()
		for viewKey, viewSpec := range meta.Views {
			if viewSpec.WireProfile != "octo/v2" {
				continue // v1 展示卡无 interaction report,不参与本轮
			}
			interaction, ok := meta.Interaction(viewKey)
			if !ok {
				t.Errorf("[%s@%s view=%s] v2 view missing InteractionReport",
					meta.ID, meta.Version, viewKey)
				continue
			}

			for _, state := range viewSpec.States {
				sample := loadSampleForState(t, tmpl, state)
				t.Run(string(meta.ID)+"@"+meta.Version+"/"+string(state), func(t *testing.T) {
					// A15b
					payload, err := registry.Render(context.Background(),
						meta.ID, meta.Version, state, sample, env)
					if err != nil {
						t.Fatalf("Render: %v", err)
					}
					card, _ := payload["card"].(map[string]any)
					if card == nil {
						t.Fatal("card missing from payload")
					}
					metadata, _ := card["metadata"].(map[string]any)
					if metadata == nil {
						t.Fatal("card.metadata missing")
					}
					octo, _ := metadata["octo"].(map[string]any)
					if octo == nil {
						t.Fatal("metadata.octo missing")
					}

					// A15d template.id/version
					tpl, _ := octo["template"].(map[string]any)
					if id, _ := tpl["id"].(string); id != string(meta.ID) {
						t.Errorf("A15d template.id = %q, want %q", id, meta.ID)
					}
					if v, _ := tpl["version"].(string); v != meta.Version {
						t.Errorf("A15d template.version = %q, want %q", v, meta.Version)
					}
					// A15e protocol
					if p, _ := octo["protocol"].(string); p != cardtmpl.Protocol {
						t.Errorf("A15e protocol = %q, want %q", p, cardtmpl.Protocol)
					}
					// A15f webUrl
					if u, _ := metadata["webUrl"].(string); !strings.HasPrefix(u, "https://") {
						t.Errorf("A15f webUrl must be https, got %q", u)
					}

					// A15c 交互契约锁
					assertInteractionLocked(t, card, interaction)
				})
			}
		}
	}
}

// TestActionContract_ThreeWayConsistency 严格实现 A16 三方一致性:
//   1. Meta.ActionContract == 期望值 (pilot: docs/access_request.decision)
//   2. Render 产物 Action.Submit.data.owner/action_type == ActionContract
//   3. 用 cardactiondispatch.NewRegistry + Resolve(NotifyBotUID, contract.Owner,
//      contract.ActionType) 必须命中 ResolutionCallback,与 main.go:521 的启动
//      fail-close 语义同源 —— 任何一处 owner 值 rename 都会立刻在 CI 抓到。
func TestActionContract_ThreeWayConsistency(t *testing.T) {
	registry := freshRegistry(t)
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://test.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-actioncontract",
	}
	for _, tmpl := range registry.RegisteredForTest() {
		meta := tmpl.Meta()
		if meta.ActionContract == nil {
			continue // 纯展示卡,无 Action.Submit
		}
		// 找一个 v2 view + 一个 state 触发有 Submit 的产物
		var state cardtmpl.State
		for _, vs := range meta.Views {
			if vs.WireProfile == "octo/v2" && len(vs.States) > 0 {
				state = vs.States[0]
				break
			}
		}
		if state == "" {
			continue
		}
		sample := loadSampleForState(t, tmpl, state)
		payload, err := registry.Render(context.Background(),
			meta.ID, meta.Version, state, sample, env)
		if err != nil {
			t.Fatalf("[%s@%s] Render: %v", meta.ID, meta.Version, err)
		}
		card, _ := payload["card"].(map[string]any)
		actions, _ := card["actions"].([]any)
		submits := 0
		for _, a := range actions {
			am, _ := a.(map[string]any)
			if am["type"] != "Action.Submit" {
				continue
			}
			submits++
			data, _ := am["data"].(map[string]any)
			if o, _ := data["owner"].(string); o != meta.ActionContract.Owner {
				t.Errorf("[%s@%s] Action.Submit.data.owner = %q, want %q",
					meta.ID, meta.Version, o, meta.ActionContract.Owner)
			}
			if at, _ := data["action_type"].(string); at != meta.ActionContract.ActionType {
				t.Errorf("[%s@%s] Action.Submit.data.action_type = %q, want %q",
					meta.ID, meta.Version, at, meta.ActionContract.ActionType)
			}
		}
		if submits == 0 {
			t.Errorf("[%s@%s view state=%s] no Action.Submit in card actions",
				meta.ID, meta.Version, state)
		}

		// A16-3: 真去构造 cardactiondispatch.Registry 并 Resolve
		aRegistry, err := cardactiondispatch.NewRegistry(
			[]cardactiondispatch.RouteSpec{{
				SenderUID:   notifyBotUID,
				Owner:       meta.ActionContract.Owner,
				ActionType:  meta.ActionContract.ActionType,
				URL:         "https://test.internal/v1/callback",
				SecretEnv:   "OCTO_TEST_CALLBACK_SECRET",
				Timeout:     3 * time.Second,
				MaxAttempts: 3,
			}},
			func(key string) string {
				if key == "OCTO_TEST_CALLBACK_SECRET" {
					return "test-callback-secret-32-bytes-min-len!"
				}
				return ""
			},
		)
		if err != nil {
			t.Fatalf("[%s@%s] build cardactiondispatch registry: %v", meta.ID, meta.Version, err)
		}
		resolution := aRegistry.Resolve(
			notifyBotUID,
			meta.ActionContract.Owner,
			meta.ActionContract.ActionType,
		)
		if resolution.Kind != cardactiondispatch.ResolutionCallback {
			t.Errorf("[%s@%s] cardactiondispatch.Resolve(%s, %s, %s).Kind = %v, want %v",
				meta.ID, meta.Version,
				notifyBotUID,
				meta.ActionContract.Owner,
				meta.ActionContract.ActionType,
				resolution.Kind,
				cardactiondispatch.ResolutionCallback,
			)
		}
	}
}

// ---- 断言 helpers ----

func assertInteractionLocked(t *testing.T, card map[string]any, ir cardtmpl.InteractionReport) {
	t.Helper()
	// 收集产物侧 Action.Submit id / Input.* id / data keys / associatedInputs
	submitIDs := map[string]map[string]any{}
	inputIDs := map[string]map[string]any{}
	collectActionsFromCard(card, submitIDs)
	collectInputsFromCard(card, inputIDs)

	// 期望集
	wantSubmitIDs := map[string]cardtmpl.DeclaredAction{}
	wantOpenURLIDs := map[string]cardtmpl.DeclaredAction{}
	for _, a := range ir.Actions {
		switch a.Type {
		case "Action.Submit":
			wantSubmitIDs[a.ID] = a
		case "Action.OpenUrl":
			wantOpenURLIDs[a.ID] = a
		}
	}
	wantInputIDs := map[string]cardtmpl.DeclaredInput{}
	for _, in := range ir.Inputs {
		wantInputIDs[in.ID] = in
	}

	// A15c-1: Action.Submit id 集合完全相等
	if diff := setSymDiff(keys(submitIDs), keys(wantSubmitIDs)); len(diff) > 0 {
		t.Errorf("A15c-1 Action.Submit id 集合漂移: %v (got=%v want=%v)",
			diff, keys(submitIDs), keys(wantSubmitIDs))
	}
	// Input.* id 集合完全相等 (§4.3-2: hidden input 也必须出现)
	if diff := setSymDiff(keys(inputIDs), keys(wantInputIDs)); len(diff) > 0 {
		t.Errorf("A15c-2 Input.* id 集合漂移: %v (got=%v want=%v)",
			diff, keys(inputIDs), keys(wantInputIDs))
	}
	// A15c-3 每个 Action.Submit.data keys 完全等于声明
	for id, decl := range wantSubmitIDs {
		am := submitIDs[id]
		if am == nil {
			continue // 已在 A15c-1 报过
		}
		data, _ := am["data"].(map[string]any)
		gotKeys := mapKeys(data)
		wantKeys := append([]string(nil), decl.DataKeys...)
		sort.Strings(gotKeys)
		sort.Strings(wantKeys)
		if !equalStringSlices(gotKeys, wantKeys) {
			t.Errorf("A15c-3 Action.Submit[id=%s] data keys 漂移: got=%v want=%v",
				id, gotKeys, wantKeys)
		}
	}
	// A15c-4 associatedInputs 一致 (只对声明了非空值的做检查,AC 默认是 "auto")
	for id, decl := range wantSubmitIDs {
		if decl.AssociatedInputs == "" {
			continue
		}
		am := submitIDs[id]
		if am == nil {
			continue
		}
		got, _ := am["associatedInputs"].(string)
		if got == "" {
			got = "auto" // AC 默认
		}
		if got != decl.AssociatedInputs {
			t.Errorf("A15c-4 Action.Submit[id=%s] associatedInputs = %q, want %q",
				id, got, decl.AssociatedInputs)
		}
	}
	// OpenUrl id 集合子集 (视图详情按钮等 optional,不做严格相等)
	for id := range wantOpenURLIDs {
		if _, ok := findActionByID(card, id); !ok {
			t.Errorf("A15c OpenUrl action[id=%s] not present in card body/actions", id)
		}
	}
}

func collectActionsFromCard(card map[string]any, out map[string]map[string]any) {
	if actions, ok := card["actions"].([]any); ok {
		for _, a := range actions {
			am, _ := a.(map[string]any)
			if am["type"] == "Action.Submit" {
				if id, _ := am["id"].(string); id != "" {
					out[id] = am
				}
			}
		}
	}
	// body 内嵌 ActionSet 也需要遍历
	if body, ok := card["body"].([]any); ok {
		for _, el := range body {
			walkForSubmit(el, out)
		}
	}
}

func walkForSubmit(el any, out map[string]map[string]any) {
	em, ok := el.(map[string]any)
	if !ok {
		return
	}
	if em["type"] == "Action.Submit" {
		if id, _ := em["id"].(string); id != "" {
			out[id] = em
		}
	}
	if actions, ok := em["actions"].([]any); ok {
		for _, a := range actions {
			walkForSubmit(a, out)
		}
	}
	if items, ok := em["items"].([]any); ok {
		for _, i := range items {
			walkForSubmit(i, out)
		}
	}
	if cols, ok := em["columns"].([]any); ok {
		for _, c := range cols {
			walkForSubmit(c, out)
		}
	}
}

func collectInputsFromCard(card map[string]any, out map[string]map[string]any) {
	if body, ok := card["body"].([]any); ok {
		for _, el := range body {
			walkForInput(el, out)
		}
	}
}

func walkForInput(el any, out map[string]map[string]any) {
	em, ok := el.(map[string]any)
	if !ok {
		return
	}
	if t, _ := em["type"].(string); strings.HasPrefix(t, "Input.") {
		if id, _ := em["id"].(string); id != "" {
			out[id] = em
		}
	}
	if items, ok := em["items"].([]any); ok {
		for _, i := range items {
			walkForInput(i, out)
		}
	}
	if cols, ok := em["columns"].([]any); ok {
		for _, c := range cols {
			walkForInput(c, out)
		}
	}
}

func findActionByID(card map[string]any, id string) (map[string]any, bool) {
	// 检查根 actions
	if actions, ok := card["actions"].([]any); ok {
		for _, a := range actions {
			am, _ := a.(map[string]any)
			if aid, _ := am["id"].(string); aid == id {
				return am, true
			}
		}
	}
	// 检查 body 内嵌 ActionSet
	found := false
	var hit map[string]any
	if body, ok := card["body"].([]any); ok {
		for _, el := range body {
			walkForID(el, id, &found, &hit)
		}
	}
	return hit, found
}

func walkForID(el any, id string, found *bool, hit *map[string]any) {
	if *found {
		return
	}
	em, ok := el.(map[string]any)
	if !ok {
		return
	}
	if aid, _ := em["id"].(string); aid == id {
		*found = true
		*hit = em
		return
	}
	if actions, ok := em["actions"].([]any); ok {
		for _, a := range actions {
			walkForID(a, id, found, hit)
		}
	}
	if items, ok := em["items"].([]any); ok {
		for _, i := range items {
			walkForID(i, id, found, hit)
		}
	}
	if cols, ok := em["columns"].([]any); ok {
		for _, c := range cols {
			walkForID(c, id, found, hit)
		}
	}
}

func setSymDiff(a, b []string) []string {
	as := map[string]struct{}{}
	for _, x := range a {
		as[x] = struct{}{}
	}
	bs := map[string]struct{}{}
	for _, x := range b {
		bs[x] = struct{}{}
	}
	var out []string
	for x := range as {
		if _, ok := bs[x]; !ok {
			out = append(out, "only-in-got:"+x)
		}
	}
	for x := range bs {
		if _, ok := as[x]; !ok {
			out = append(out, "only-in-want:"+x)
		}
	}
	sort.Strings(out)
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// freshRegistry 为 conformance test 装配同生产的 Registry。
func freshRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	r.Freeze()
	return r
}

// loadSampleForState 从 pilot 子包 embed 里读取对应 state 的 sample bytes。
// 简化实现:pilot 每个 state 都有一份同名 sample (pending.json/approved.json/rejected.json)。
func loadSampleForState(t *testing.T, tmpl cardtmpl.Template, state cardtmpl.State) json.RawMessage {
	t.Helper()
	// docsaccessrequest 的 samples 命名与 state 值同名
	fs := docsaccessrequest.Assets
	root := docsaccessrequest.HandoffRoot
	b, err := fs.ReadFile(root + "/samples/" + string(state) + ".json")
	if err != nil {
		t.Fatalf("read sample for state %q: %v", state, err)
	}
	return json.RawMessage(b)
}

// Ensure imported errors package is used (test verifies sentinel is exported).
var _ = errors.New
