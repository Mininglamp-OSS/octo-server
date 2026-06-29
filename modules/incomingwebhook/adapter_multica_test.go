package incomingwebhook

import (
	"encoding/json"
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
	// 多行渲染：
	//   **MUL-123** Fix login redirect on mobile
	//   状态: `todo` → `in_progress`
	//   触发: agent
	assert.Contains(t, req.Content, "**MUL-123**")
	assert.Contains(t, req.Content, "Fix login redirect on mobile")
	assert.Contains(t, req.Content, "状态: `todo` → `in_progress`")
	assert.Contains(t, req.Content, "触发: agent")
	assert.Empty(t, req.MsgType, "adapters emit the plain-text path")
}

func TestParseMulticaPush_IssueStatusChanged_WithLinkAndAssignee(t *testing.T) {
	// multica 富集 issue_url + assignee_name 时：标题成可点链接，多一行指派。
	body := []byte(`{
		"event": "issue.status_changed",
		"workspace_id": "550e8400-e29b-41d4-a716-446655440000",
		"actor": {"type": "agent", "id": "agent-7"},
		"issue": {
			"identifier": "MUL-123",
			"title": "Fix login redirect",
			"status": "in_progress"
		},
		"issue_url": "https://app.multica.ai/acme/issues/MUL-123",
		"assignee_type": "member",
		"assignee_name": "张三",
		"previous_status": "todo"
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	// 标题行渲染成 markdown 链接，链接文本是 "identifier title"。
	assert.Contains(t, req.Content, "**[MUL-123 Fix login redirect](https://app.multica.ai/acme/issues/MUL-123)**")
	// 指派(带 type)与触发同在一行，用 " · " 连接。
	assert.Contains(t, req.Content, "指派: 张三 (member) · 触发: agent")
}

func TestParseMulticaPush_AssigneeWithoutActor(t *testing.T) {
	// 只有 assignee_name、没有 actor.type：指派行只渲染指派段，不带 " · 触发"。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-5", "title": "x", "status": "done"},
		"assignee_type": "agent",
		"assignee_name": "Codex Bot",
		"previous_status": "todo"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "指派: Codex Bot")
	assert.NotContains(t, req.Content, "·", "no separator when actor is absent")
	assert.NotContains(t, req.Content, "触发:")
}

func TestParseMulticaPush_MaliciousIssueURLDowngraded(t *testing.T) {
	// issue_url 必须过 http(s) 白名单：javascript: 等 scheme 不能渲染成链接，
	// 标题降级为纯文本（identifier 加粗），且恶意 url 不出现在 content 里。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-9", "title": "evil", "status": "done"},
		"issue_url": "javascript:alert(1)",
		"previous_status": "todo"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.NotContains(t, req.Content, "javascript:", "non-http(s) url must not be rendered")
	assert.NotContains(t, req.Content, "](", "url must not become a markdown link")
	assert.Contains(t, req.Content, "**MUL-9**", "title falls back to bold plain text")
}

func TestParseMulticaPush_NoIssueURL_PlainTextTitle(t *testing.T) {
	// 无 issue_url（旧版 multica）：标题回退纯文本 `**id** title`，不是链接。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-4", "title": "no link", "status": "done"},
		"previous_status": "todo"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "**MUL-4** no link")
	assert.NotContains(t, req.Content, "](", "no link without issue_url")
}

// TestParseMulticaPush_IssueURLDestinationInjection is the #496 blocker regression:
// a URL that passes a naive http(s) prefix check but carries link-breaking
// characters (`)`, space, etc.) must NOT be rendered as a link — it would close
// the intended destination and inject a second attacker-controlled link.
func TestParseMulticaPush_IssueURLDestinationInjection(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"close_and_inject", "https://ok.com/) [phish](https://evil.com"},
		{"trailing_paren", "https://ok.com/issue)"},
		{"embedded_space", "https://ok.com/a b"},
		{"newline", "https://ok.com/a\nb"},
		{"angle_brackets", "https://ok.com/<script>"},
		{"bracket", "https://ok.com/]injected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"event":           "issue.status_changed",
				"issue":           map[string]any{"identifier": "MUL-1", "title": "hi", "status": "done"},
				"issue_url":       tc.url,
				"previous_status": "todo",
			})
			require.NoError(t, err)
			req, _, _ := parseMulticaPush(http.Header{}, body)
			require.NotNil(t, req)
			// The malicious destination must never be rendered as a markdown link.
			assert.NotContains(t, req.Content, "](", "unsafe url must not become a markdown link: %q", req.Content)
			assert.NotContains(t, req.Content, "[phish]", "second injected link must not appear")
			// Degrades to a plain-text bold identifier instead.
			assert.Contains(t, req.Content, "**MUL-1**", "title falls back to bold plain text")
		})
	}
}

// TestParseMulticaPush_LinkTextInjection is the #496 second blocker: when the
// title IS rendered inside a link (`[text](url)`), CommonMark still parses
// emphasis/code spans in the text, so a title with `**` or a backtick must be
// neutralized — the link-text path uses mdInertText (escapes `*`, strips
// backtick), not mdLinkText (which only escapes `\[]`).
func TestParseMulticaPush_LinkTextInjection(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"event":           "issue.status_changed",
		"issue":           map[string]any{"identifier": "MUL-1", "title": "a **bold** `tick", "status": "in_progress"},
		"issue_url":       "https://app.multica.ai/acme/issues/MUL-1",
		"previous_status": "todo",
	})
	require.NoError(t, err)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	// asterisks escaped → no injected bold inside the link text.
	assert.Contains(t, req.Content, `\*\*bold\*\*`, "asterisks in link text must be escaped")
	assert.NotContains(t, req.Content, "a **bold**", "raw bold must not survive in link text")
	// Backticks remain balanced (the title's lone backtick is stripped), so the
	// downstream status code-span is not corrupted.
	assert.Equalf(t, 0, strings.Count(req.Content, "`")%2,
		"backticks must remain balanced (got odd count): %q", req.Content)
	// Still a valid single link to the safe destination.
	assert.Contains(t, req.Content, "](https://app.multica.ai/acme/issues/MUL-1)")
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
	assert.Contains(t, req.Content, "触发: member")
}

func TestParseMulticaPush_NoActorType(t *testing.T) {
	// actor.type 缺失且无 assignee：整个指派/触发行省略。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-3", "title": "no actor", "status": "done"},
		"previous_status": "in_progress"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.NotContains(t, req.Content, "触发:")
	assert.NotContains(t, req.Content, "指派:")
}

func TestParseMulticaPush_TitleEscaping(t *testing.T) {
	// 标题里出现 `[` / `]`：mdInertText 会把 `[` / `]` 转义成 `\[` / `\]`，
	// 即便未来 title 改为渲染成链接文本也安全。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-77", "title": "Crash on [enter] key", "status": "done"},
		"previous_status": "in_progress"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.Contains(t, req.Content, `\[enter\]`, "brackets must be markdown-escaped")
}

// title 在「**identifier** title:」上下文是裸文本（不在 `[text](url)` 链接里），
// 必须用 mdInertText 而非 mdLinkText 才能把 `*` / 反引号 / `<` / `|` 等元字符
// 也防住。yujiawei review P1 给出了 4 个具体注入向量，每个要 hold。
func TestParseMulticaPush_TitleInjectionVectors(t *testing.T) {
	cases := []struct {
		name        string
		title       string
		mustContain []string // 修复后期望出现的转义序列
		mustNot     []string // 修复前漏掉的原始注入串
	}{
		{
			name:        "asterisk_pair_injects_bold",
			title:       "**hijack**",
			mustContain: []string{`\*\*hijack\*\*`},
			mustNot:     []string{"**hijack**"},
		},
		{
			name:    "backtick_breaks_status_code_span",
			title:   "a `b",
			mustNot: []string{"a `b"}, // 反引号必须被剥（mdInertText 行为），否则总反引号数变奇数
		},
		{
			name:        "html_autolink",
			title:       "<script>alert(1)</script>",
			mustContain: []string{`\<script\>`, `\</script\>`},
		},
		{
			name:        "pipe_breaks_tables",
			title:       "a | b",
			mustContain: []string{`a \| b`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"event":           "issue.status_changed",
				"actor":           map[string]any{"type": "member", "id": "u-1"},
				"issue":           map[string]any{"identifier": "MUL-1", "title": tc.title, "status": "in_progress"},
				"previous_status": "todo",
			})
			require.NoError(t, err)
			req, _, _ := parseMulticaPush(http.Header{}, body)
			require.NotNil(t, req)
			for _, want := range tc.mustContain {
				assert.Containsf(t, req.Content, want,
					"escape must produce %q; got %q", want, req.Content)
			}
			for _, bad := range tc.mustNot {
				assert.NotContainsf(t, req.Content, bad,
					"raw injection sequence %q must not survive; got %q", bad, req.Content)
			}
			// 总反引号数始终偶数（成对开闭）—— mdCodeSpanText 已经在 status 字段
			// 把反引号剥光；现在 title 也走 mdInertText 同样剥光，所以一个含反
			// 引号的 title 不会让 content 反引号总数变奇数破坏 status code-span。
			assert.Equalf(t, 0, strings.Count(req.Content, "`")%2,
				"backticks must remain balanced (got odd count): %q", req.Content)
		})
	}
}

// 事件名 lower+trim 后匹配——与 msg_type 处理保持一致，避免大小写变体被静默
// 折叠成 skip(event)（yujiawei review P2-2）。
func TestParseMulticaPush_EventCaseInsensitive(t *testing.T) {
	for _, name := range []string{"Issue.Status_Changed", "ISSUE.STATUS_CHANGED", " issue.status_changed "} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"event":           name,
				"actor":           map[string]any{"type": "member", "id": "u-1"},
				"issue":           map[string]any{"identifier": "MUL-1", "title": "x", "status": "done"},
				"previous_status": "todo",
			})
			require.NoError(t, err)
			req, skip, invalid := parseMulticaPush(http.Header{}, body)
			require.NotNilf(t, req, "case-variant event must render; skip=%q invalid=%q", skip, invalid)
		})
	}
}

// 非链接上下文的字段（identifier 在 bold、actor.type 在 plain text）必须经
// mdInertText 转义；status 在 backtick code span 走 mdCodeSpanText 单独 strip
// 反引号。否则一个带 `*` 的 identifier 可以提前闭合 **...**、带反引号的 status
// 可以逃出 code span、带 `[...](url)` 的 actor 可以注入链接。三位 reviewer
// (Jerry-Xin / yujiawei / mochashanyao) 都点了这道防御。
func TestParseMulticaPush_IdentifierMarkdownEscaping(t *testing.T) {
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-1** [phish](http://evil.com) **", "title": "ok", "status": "done"},
		"previous_status": "todo"
	}`)
	req, skip, invalid := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	// `*` 必须被反斜杠转义，否则会提前关闭 **...** 的粗体并让后面的 `[phish](...)`
	// 渲染成 markdown 链接。
	assert.Contains(t, req.Content, `\*\*`,
		"asterisks in identifier must be escaped to prevent bold-break + link injection")
	assert.Contains(t, req.Content, `\[phish\]`,
		"square brackets in identifier must be escaped to prevent link injection")
	assert.NotContains(t, req.Content, `[phish](`,
		"un-escaped link syntax must not leak through")
}

func TestParseMulticaPush_StatusBacktickStripping(t *testing.T) {
	// status 渲染在 `...` code span 内；嵌入反引号无法用 \\` 安全转义
	// （单 backtick fence 里 \\` 仍是字面反斜杠），按契约 strip 反引号
	// （mdCodeSpanText 行为）。
	body := []byte(`{
		"event": "issue.status_changed",
		"issue": {"identifier": "MUL-1", "title": "x", "status": "in_pro` + "`" + `gress"},
		"previous_status": "to` + "`" + `do"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	// 反引号被剥后 status="in_progress"、prev="todo"，渲染成 `todo` → `in_progress`。
	// 关键不变量：原始反引号不能在最终 content 中以「跨 code-span 的 ` 字符」形式
	// 出现，否则就破坏了 code span。
	assert.Contains(t, req.Content, "`todo` → `in_progress`",
		"backticks inside status/previous_status must be stripped, restoring the normal display")
	// 进一步钉一下：内容里 ` 的总数必须是偶数（成对开闭 code span），
	// 不应因 strip 出现奇数个反引号导致开/闭不平衡。
	assert.Equalf(t, 0, strings.Count(req.Content, "`")%2,
		"backticks must remain balanced (got odd count): %q", req.Content)
}

func TestParseMulticaPush_ActorTypeMarkdownEscaping(t *testing.T) {
	// actor.type 在 `(by X)` 纯文本上下文；带 `[label](url)` 的恶意值必须
	// 不能拼出可点链接。
	body := []byte(`{
		"event": "issue.status_changed",
		"actor": {"type": "agent [click](http://evil.com)", "id": "a-1"},
		"issue": {"identifier": "MUL-2", "title": "x", "status": "done"},
		"previous_status": "todo"
	}`)
	req, _, _ := parseMulticaPush(http.Header{}, body)
	require.NotNil(t, req)
	assert.Contains(t, req.Content, `\[click\]`,
		"square brackets in actor.type must be escaped to prevent link injection")
	assert.NotContains(t, req.Content, `(click)(http`,
		"the un-escaped link syntax must not survive")
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
	// 与 github 适配器缺 X-GitHub-Event 头同语义——deliveries 里 reason=no_event
	// 让运维一眼区分「配置错误」vs「事件不在渲染子集内」(yujiawei review P2-2)。
	assert.Equal(t, "no_event", invalid)
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
