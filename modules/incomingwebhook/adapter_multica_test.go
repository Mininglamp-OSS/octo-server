package incomingwebhook

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Multica 适配器纯翻译单测（无 DB/Redis/IM 依赖）。fixture 取自 multica
// outboundPayload（server/internal/integrations/outwebhook/dispatcher.go）的
// 字段子集——mu* 结构体本就是白名单解析，多余字段一律忽略。

func TestParseMulticaPush_IssueStatusChanged_FullEnvelope(t *testing.T) {
	body := []byte(`{
		"event": "issue.status_changed",
		"workspace_id": "550e8400-e29b-41d4-a716-446655440000",
		"actor": {"type": "agent", "id": "agent-7"},
		"issue": {
			"identifier": "MUL-123",
			"title": "Fix login redirect on mobile",
			"status": "in_progress"
		},
		"previous_status": "todo",
		"delivered_at": "2026-06-22T14:30:45Z"
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	// 期望渲染：**MUL-123** Fix login redirect on mobile: `todo` → `in_progress` (by agent)
	assert.Contains(t, req.Content, "**MUL-123**")
	assert.Contains(t, req.Content, "Fix login redirect on mobile")
	assert.Contains(t, req.Content, "`todo` → `in_progress`")
	assert.Contains(t, req.Content, "(by agent)")
	assert.Empty(t, req.MsgType, "adapters emit the plain-text path")
}

func TestParseMulticaPush_IssueStatusChanged_NoPreviousStatus(t *testing.T) {
	// previous_status 缺失（或与当前相同）：不渲染 "→ X" 尾巴。
	body := []byte(`{
		"event": "issue.status_changed",
		"actor": {"type": "member", "id": "u-1"},
		"issue": {"identifier": "MUL-9", "title": "First issue", "status": "todo"}
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Contains(t, req.Content, "**MUL-9**")
	assert.Contains(t, req.Content, "First issue")
	assert.Contains(t, req.Content, "`todo`")
	assert.NotContains(t, req.Content, "→", "no arrow when previous_status is absent")
	assert.Contains(t, req.Content, "(by member)")
}

func TestParseMulticaPush_NoActorType(t *testing.T) {
	// actor.type 缺失：不渲染 "(by …)" 尾巴。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-3", "title": "no actor", "status": "done"},
		"previous_status": "in_progress"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.NotContains(t, req.Content, "(by")
}

func TestParseMulticaPush_TitleEscaping(t *testing.T) {
	// 标题里出现 `[` / `]`：未来若改为渲染成链接文本必须转义，现在已先转义防御。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-77", "title": "Crash on [enter] key", "status": "done"},
		"previous_status": "in_progress"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.Contains(t, req.Content, `\[enter\]`, "brackets must be markdown-escaped (mdLinkText)")
}

func TestParseMulticaPush_LongTitleIsClipped(t *testing.T) {
	// 标题字段由 multica 端控制，长度理论上不受 8KB body cap 约束（短信封下仍能塞下
	// 几 KB title）；adapter 必须把过长内容钳到 mdLinkText 的 200 rune 范围内，
	// 避免无意义刷屏。
	longTitle := strings.Repeat("a", 500)
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-1", "title": "` + longTitle + `", "status": "done"},
		"previous_status": "todo"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	// 收尾应是省略号；总长不应超过钳值（200 rune + 包装）+ 余量
	assert.Contains(t, req.Content, "…")
	assert.LessOrEqual(t, len([]rune(req.Content)), 260)
}

func TestParseMulticaPush_UnknownEventIsSkipped(t *testing.T) {
	body := []byte(`{
		"event": "issue.created",
		"issue": {"identifier": "MUL-1", "title": "new", "status": "todo"}
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	assert.Nil(t, req)
	assert.Equal(t, "event", skip, "未识别事件 → 200 + auditSkipped(reason=event)，与 github 适配器对称")
	assert.Empty(t, invalid)
}

func TestParseMulticaPush_MissingEvent(t *testing.T) {
	body := []byte(`{"issue": {"identifier": "MUL-1", "title": "x", "status": "todo"}}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	assert.Nil(t, req)
	assert.Empty(t, skip)
	assert.Equal(t, "json", invalid, "缺 event 字段按 json 拒绝（不是合法 multica 出站流量）")
}

func TestParseMulticaPush_MalformedJSON(t *testing.T) {
	req, skip, invalid := parseMulticaPush(http.Header{}, []byte(`{not json`))
	assert.Nil(t, req)
	assert.Empty(t, skip)
	assert.Equal(t, "json", invalid)
}

func TestParseMulticaPush_MissingIssueIdentifier(t *testing.T) {
	// identifier 是渲染最小集，缺失就生成不出可读内容：按 content 拒绝。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"title": "no id", "status": "todo"}
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	assert.Nil(t, req)
	assert.Empty(t, skip)
	assert.Equal(t, "content", invalid)
}

func TestParseMulticaPush_MissingIssueStatus(t *testing.T) {
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-1", "title": "no status"}
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	assert.Nil(t, req)
	assert.Empty(t, skip)
	assert.Equal(t, "content", invalid)
}

func TestMulticaAdapter_AdapterRegistration(t *testing.T) {
	// 钉一下 adapter 全局变量没被改名：name、bodyLimit、parse 必须齐全。
	assert.Equal(t, adapterMultica, multicaAdapter.name)
	assert.NotNil(t, multicaAdapter.parse)
	assert.NotNil(t, multicaAdapter.bodyLimit)
	assert.Empty(t, multicaAdapter.successExtra, "multica adapter 不附带平台兼容字段")
}

func TestPublicURLs_HasMulticaEntry(t *testing.T) {
	urls := publicURLs("iwh_abc", "deadbeef")
	require.Contains(t, urls, "multica")
	assert.Equal(t, "/v1/incoming-webhooks/iwh_abc/deadbeef/multica", urls["multica"])
}
