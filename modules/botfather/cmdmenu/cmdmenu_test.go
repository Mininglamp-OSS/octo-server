package cmdmenu

import (
	"encoding/json"
	"reflect"
	"testing"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestMenuCompleteness asserts the template tree and the entries table cannot
// drift: every entry key resolves in every supported language (non-empty), and
// the tree defines no orphan keys the menu never uses.
func TestMenuCompleteness(t *testing.T) {
	wantKeys := map[string]struct{}{}
	for _, e := range entries {
		if _, dup := wantKeys[e.key]; dup {
			t.Fatalf("duplicate entry key %q", e.key)
		}
		wantKeys[e.key] = struct{}{}
	}

	gotNames := catalog.Names()
	if len(gotNames) != len(wantKeys) {
		t.Errorf("template tree defines %d keys, entries table has %d", len(gotNames), len(wantKeys))
	}
	for _, name := range gotNames {
		if _, ok := wantKeys[name]; !ok {
			t.Errorf("orphan template key %q: defined in templates but absent from entries", name)
		}
	}

	for _, lang := range octoi18n.SupportedLanguages() {
		for _, e := range entries {
			desc, err := catalog.Render(e.key, lang, nil)
			if err != nil {
				t.Errorf("render %s (%s): %v", e.key, lang, err)
				continue
			}
			if desc == "" {
				t.Errorf("render %s (%s): empty description", e.key, lang)
			}
		}
	}
}

// TestZhCNWordingUnchanged pins the #335 acceptance criterion: the zh-CN menu
// must be byte-identical (entries and order included) to what
// registerBotFatherCommands hardcoded before the migration.
func TestZhCNWordingUnchanged(t *testing.T) {
	want := []Command{
		{"/install", "安装/更新 Octo 插件"},
		{"/quickstart", "AI Agent 快速入门"},
		{"/newbot", "创建新机器人"},
		{"/mybots", "查看我的机器人"},
		{"/connect", "获取连接 prompt"},
		{"/disconnect", "断开 Agent 连接"},
		{"/setname", "修改机器人名称"},
		{"/setdescription", "修改机器人描述"},
		{"/deletebot", "删除机器人"},
		{"/token", "查看 Token"},
		{"/revoke", "重置 Token"},
		{"/approve", "通过好友申请"},
		{"/reject", "拒绝好友申请"},
		{"/pending", "查看待审批好友申请"},
		{"/help", "显示帮助"},
		{"/cancel", "取消当前操作"},
	}
	if got := Commands("zh-CN"); !reflect.DeepEqual(got, want) {
		t.Errorf("zh-CN menu drifted from the pre-migration wording:\ngot  %+v\nwant %+v", got, want)
	}
}

// TestJSONWireShape asserts JSON() keeps the exact storage/wire shape clients
// already parse: an array of {command, description} objects in menu order.
func TestJSONWireShape(t *testing.T) {
	for _, lang := range octoi18n.SupportedLanguages() {
		var got []Command
		if err := json.Unmarshal([]byte(JSON(lang)), &got); err != nil {
			t.Fatalf("JSON(%s) is not valid JSON: %v", lang, err)
		}
		if !reflect.DeepEqual(got, Commands(lang)) {
			t.Errorf("JSON(%s) does not round-trip to Commands(%s)", lang, lang)
		}
	}
}

// TestJSONStoredByteShapeUnchanged pins JSON("zh-CN") byte-for-byte against
// the pre-migration json.Marshal([]map[string]string{...}) output that clients
// parse and the startup write persists — a struct-tag or marshaling change
// that drifts the stored bytes must fail here even if the decoded values
// still match.
func TestJSONStoredByteShapeUnchanged(t *testing.T) {
	want := `[{"command":"/install","description":"安装/更新 Octo 插件"},` +
		`{"command":"/quickstart","description":"AI Agent 快速入门"},` +
		`{"command":"/newbot","description":"创建新机器人"},` +
		`{"command":"/mybots","description":"查看我的机器人"},` +
		`{"command":"/connect","description":"获取连接 prompt"},` +
		`{"command":"/disconnect","description":"断开 Agent 连接"},` +
		`{"command":"/setname","description":"修改机器人名称"},` +
		`{"command":"/setdescription","description":"修改机器人描述"},` +
		`{"command":"/deletebot","description":"删除机器人"},` +
		`{"command":"/token","description":"查看 Token"},` +
		`{"command":"/revoke","description":"重置 Token"},` +
		`{"command":"/approve","description":"通过好友申请"},` +
		`{"command":"/reject","description":"拒绝好友申请"},` +
		`{"command":"/pending","description":"查看待审批好友申请"},` +
		`{"command":"/help","description":"显示帮助"},` +
		`{"command":"/cancel","description":"取消当前操作"}]`
	if got := JSON("zh-CN"); got != want {
		t.Errorf("JSON(\"zh-CN\") stored byte shape drifted:\ngot  %s\nwant %s", got, want)
	}
}

// TestLanguageFallback asserts unsupported/empty tags fall back to the source
// language and region-less tags match their supported variant, mirroring the
// msgtmpl normalization contract.
func TestLanguageFallback(t *testing.T) {
	src := JSON(octoi18n.SourceLanguage)
	for _, lang := range []string{"", "fr-FR", "not-a-lang"} {
		if got := JSON(lang); got != src {
			t.Errorf("JSON(%q) should fall back to source language", lang)
		}
	}
	if got := JSON("zh"); got != JSON("zh-CN") {
		t.Errorf("JSON(\"zh\") should match the zh-CN variant")
	}
	if got := JSON("en"); got != JSON("en-US") {
		t.Errorf("JSON(\"en\") should match the en-US variant")
	}
}

// TestCommandsReturnsCopy guards the package-level cache against caller
// mutation — the pre-rendered slices back every request.
func TestCommandsReturnsCopy(t *testing.T) {
	first := Commands("zh-CN")
	first[0].Description = "mutated"
	if second := Commands("zh-CN"); second[0].Description == "mutated" {
		t.Error("Commands must return a copy, not the cached slice")
	}
}
