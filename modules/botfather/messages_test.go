package botfather

import (
	"sort"
	"strings"
	"testing"
	"time"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// renderProbe carries every field any BotFather template interpolates, so a
// single value can drive the completeness render of every key regardless of
// which fields that key actually uses (text/template ignores the extras).
func renderProbe() map[string]any {
	return map[string]any{
		"Name":          "probe",
		"BotDisplay":    "probe",
		"BotID":         "probe",
		"Token":         "probe",
		"APIKey":        "probe",
		"APIURL":        "https://probe",
		"DisplayName":   "probe",
		"RobotID":       "probe",
		"BotToken":      "probe",
		"Prompt":        "probe",
		"ApplicantName": "probe",
		"ApplicantUID":  "probe",
		"RobotUID":      "probe",
		"ApplyName":     "probe",
		"ApplyUID":      "probe",
		"Remark":        "probe",
		"Ago":           "probe",
		"Count":         1,
		"N":             1,
		"Bots":          []botListItem{{Num: 1, Display: "probe", Desc: "probe"}},
		"Items":         []pendingApplyItem{{Num: 1, ApplyName: "probe", ApplyUID: "probe", RobotID: "probe", Ago: "probe", Remark: "probe"}},
	}
}

// TestKeyMessageInterpolation verifies that the parameterized messages actually
// interpolate their data (params land in the body, optional sections appear),
// and logs the full rendered output for manual format review.
func TestKeyMessageInterpolation(t *testing.T) {
	cases := []struct {
		key  string
		lang string
		data map[string]any
		want []string // substrings that must appear
	}{
		{MsgCreatedPrompt, "en-US", map[string]any{"Name": "Alice", "RobotID": "bot_42", "BotToken": "tok_xyz", "APIURL": "https://api.x"},
			[]string{"Alice", "bot_42", "tok_xyz", "https://api.x", "npx -y create-openclaw-octo bind", "```"}},
		{MsgCreatedPrompt, "zh-CN", map[string]any{"Name": "小爱", "RobotID": "bot_42", "BotToken": "tok_xyz", "APIURL": "https://api.x"},
			[]string{"小爱", "bot_42", "绑定"}},
		{MsgNotifyOwnerNewApply, "en-US", map[string]any{"ApplicantName": "Bob", "ApplicantUID": "u_9", "RobotUID": "bot_1", "Remark": "please"},
			[]string{"Bob", "u_9", "bot_1", "Note: please"}},
		{MsgNotifyOwnerNewApply, "en-US", map[string]any{"ApplicantName": "Bob", "ApplicantUID": "u_9", "RobotUID": "bot_1", "Remark": ""},
			[]string{"Bob", "bot_1"}}, // no Note line when remark empty
		{MsgFriendApplyNotify, "zh-CN", map[string]any{"ApplyName": "张三", "ApplyUID": "u_3", "RobotID": "bot_7", "Remark": ""},
			[]string{"张三", "/approve u_3 bot_7", "/reject u_3 bot_7"}},
		{MsgTokenRevoked, "en-US", map[string]any{"Token": "new_tok"},
			[]string{"new_tok", "old token is now invalid"}},
		{MsgFriendAgoDays, "en-US", map[string]any{"N": 1}, []string{"1 day ago"}},
		{MsgFriendAgoDays, "en-US", map[string]any{"N": 3}, []string{"3 days ago"}},
		{MsgFriendAgoDays, "zh-CN", map[string]any{"N": 3}, []string{"3天前"}},
	}
	for _, c := range cases {
		got, err := botMessages.Render(c.key, c.lang, c.data)
		if err != nil {
			t.Errorf("Render(%q,%q): %v", c.key, c.lang, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("Render(%q,%q) missing %q in:\n%s", c.key, c.lang, w, got)
			}
		}
		t.Logf("\n----- %s / %s -----\n%s\n", c.key, c.lang, got)
	}
}

// TestNotifyOwnerNewApplyNoRemarkLine guards that the optional remark section is
// omitted (no dangling "Note:"/"备注") when remark is empty.
func TestNotifyOwnerNewApplyNoRemarkLine(t *testing.T) {
	got, err := botMessages.Render(MsgNotifyOwnerNewApply, "zh-CN", map[string]any{
		"ApplicantName": "X", "ApplicantUID": "u", "RobotUID": "b", "Remark": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "备注") {
		t.Errorf("empty remark must not render the 备注 line:\n%s", got)
	}
}

// TestRelativeAgo pins the plural dispatch of the relative-time phrase
// independently of the /pending list render: singular vs plural for en-US, the
// no-plural zh-CN form, and each time bucket including just-now.
func TestRelativeAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		lang    string
		created time.Time
		want    string
	}{
		{"en days plural", "en-US", now.Add(-73 * time.Hour), "3 days ago"},
		{"en days singular", "en-US", now.Add(-25 * time.Hour), "1 day ago"},
		{"zh days", "zh-CN", now.Add(-73 * time.Hour), "3天前"},
		{"en hours plural", "en-US", now.Add(-2 * time.Hour), "2 hours ago"},
		{"en hours singular", "en-US", now.Add(-65 * time.Minute), "1 hour ago"},
		{"en minutes plural", "en-US", now.Add(-5 * time.Minute), "5 minutes ago"},
		{"en minutes singular", "en-US", now.Add(-90 * time.Second), "1 minute ago"},
		{"en just now", "en-US", now.Add(-10 * time.Second), "just now"},
		{"zh just now", "zh-CN", now.Add(-10 * time.Second), "刚刚"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := relativeAgo(c.lang, c.created.Unix())
			if got != c.want {
				t.Errorf("relativeAgo(%q, %s ago) = %q, want %q", c.lang, c.name, got, c.want)
			}
		})
	}
}

// TestBotMessagesCompleteness is the build-time guarantee behind #304's
// acceptance: every key the code renders resolves in every supported language,
// and the template tree defines exactly the declared key set (no orphan
// template, no missing key). MustNew already enforces cross-language name
// parity at init; this adds code↔template parity and per-key renderability.
func TestBotMessagesCompleteness(t *testing.T) {
	probe := renderProbe()
	for _, key := range allBotMessageKeys {
		for _, lang := range octoi18n.SupportedLanguages() {
			got, err := botMessages.Render(key, lang, probe)
			if err != nil {
				t.Errorf("Render(%q, %q) failed: %v", key, lang, err)
				continue
			}
			if got == "" {
				t.Errorf("Render(%q, %q) returned empty", key, lang)
			}
		}
	}

	got := botMessages.Names()
	want := append([]string(nil), allBotMessageKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("template names (%d) != declared keys (%d)\n  templates: %v\n  declared:  %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("template/declared key mismatch at %d: %q vs %q\n  templates: %v\n  declared:  %v", i, got[i], want[i], got, want)
		}
	}
}
