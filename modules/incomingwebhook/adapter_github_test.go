package incomingwebhook

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub 适配器纯翻译单测（无 DB/Redis/IM 依赖）。fixture 取自 GitHub webhook 文档的
// 字段子集——gh* 结构体本就是白名单解析，多余字段与缺省字段都不影响结果。

func ghHeader(event string) http.Header {
	h := http.Header{}
	if event != "" {
		h.Set("X-GitHub-Event", event)
	}
	return h
}

func TestParseGitHubPush_HeaderGate(t *testing.T) {
	t.Run("missing event header is invalid", func(t *testing.T) {
		req, skip, invalid := parseGitHubPush(http.Header{}, []byte(`{}`))
		assert.Nil(t, req)
		assert.Empty(t, skip)
		// 缺事件头是配置错误 → 独立 no_event，与「渲染子集之外」的 200 skipped(event)
		// 分开，deliveries 里只看 reason 即可分辨二者（PR #330 review 跟进）。
		assert.Equal(t, "no_event", invalid)
	})
	t.Run("ping is skipped", func(t *testing.T) {
		req, skip, invalid := parseGitHubPush(ghHeader("ping"), []byte(`{"zen":"Design for failure."}`))
		assert.Nil(t, req)
		assert.Equal(t, "ping", skip)
		assert.Empty(t, invalid)
	})
	t.Run("unsupported event is skipped", func(t *testing.T) {
		req, skip, invalid := parseGitHubPush(ghHeader("watch"), []byte(`{"action":"started"}`))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
		assert.Empty(t, invalid)
	})
	t.Run("malformed body is invalid json", func(t *testing.T) {
		req, skip, invalid := parseGitHubPush(ghHeader("push"), []byte(`{not json`))
		assert.Nil(t, req)
		assert.Empty(t, skip)
		assert.Equal(t, "json", invalid)
	})
}

func TestParseGitHubPush_PushEvent(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"commits": [
			{"id": "aaaabbbbccccdddd", "message": "feat: first\n\nbody", "url": "https://github.com/o/r/commit/aaaabbbb"},
			{"id": "1111222233334444", "message": "fix: second", "url": "https://github.com/o/r/commit/11112222"}
		],
		"repository": {"full_name": "octo/repo", "html_url": "https://github.com/octo/repo"},
		"sender": {"login": "alice"}
	}`)
	req, skip, invalid := parseGitHubPush(ghHeader("push"), body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Contains(t, req.Content, "**alice** pushed 2 commit(s) to `main`")
	assert.Contains(t, req.Content, "[octo/repo](https://github.com/octo/repo)")
	assert.Contains(t, req.Content, "[`aaaabbb`](https://github.com/o/r/commit/aaaabbbb) feat: first")
	assert.NotContains(t, req.Content, "body", "only the first line of a commit message is rendered")
	assert.Empty(t, req.MsgType, "adapters emit the plain-text path")
}

func TestParseGitHubPush_PushVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"tag push", `{"ref":"refs/tags/v1.2.0","sender":{"login":"bob"},"repository":{"full_name":"o/r"}}`,
			"**bob** pushed tag `v1.2.0`"},
		{"tag delete", `{"ref":"refs/tags/v1.2.0","deleted":true,"sender":{"login":"bob"}}`,
			"**bob** deleted tag `v1.2.0`"},
		{"branch delete", `{"ref":"refs/heads/dev","deleted":true,"sender":{"login":"bob"}}`,
			"**bob** deleted branch `dev`"},
		{"branch create without commits", `{"ref":"refs/heads/dev","created":true,"commits":[],"sender":{"login":"bob"}}`,
			"**bob** created branch `dev`"},
		{"force push", `{"ref":"refs/heads/main","forced":true,"commits":[{"id":"abc","message":"m","url":"u"}],"sender":{"login":"bob"}}`,
			"force-pushed 1 commit(s)"},
		{"missing sender falls back", `{"ref":"refs/heads/main","commits":[{"id":"abc","message":"m","url":"u"}],"sender":{}}`,
			"**someone** pushed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, skip, invalid := parseGitHubPush(ghHeader("push"), []byte(tc.body))
			require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
			assert.Contains(t, req.Content, tc.want)
		})
	}
}

// 非 create/delete 且无提交的退化 ref 更新不渲染 "pushed 0 commit(s)"，走 skip。
func TestParseGitHubPush_NoCommitRefUpdateSkipped(t *testing.T) {
	body := `{"ref":"refs/heads/main","commits":[],"sender":{"login":"bob"}}`
	req, skip, invalid := parseGitHubPush(ghHeader("push"), []byte(body))
	assert.Nil(t, req)
	assert.Equal(t, "event", skip)
	assert.Empty(t, invalid)
}

// GitHub 事件 body 是平台生成的（普遍 >8KiB 且发送方无法修短），其上限必须宽于
// native 的调用方编写上限——钉住 review 阻断项的修复不被回退。两个 cap 的 env 都用
// t.Setenv 清空钉到各自默认值，结果不再依赖宿主机的环境变量（PR #330 review 跟进）。
func TestGitHubMaxBytes_ExceedsNativeCap(t *testing.T) {
	t.Setenv(envBodyMax, "")
	t.Setenv(envGitHubBodyMax, "")
	assert.Greater(t, githubMaxBytes(), maxBytes())
	assert.Equal(t, defaultGitHubMaxBytes, githubMaxBytes())
}

// env 被手误填成天文数字时，githubMaxBytes 钳到 25MiB 硬顶，不让单请求 body 缓冲被
// 放大到危险量级；合理的自定义值（如 4MiB）仍原样生效（PR #330 review 跟进）。
func TestGitHubMaxBytes_ClampsHugeEnv(t *testing.T) {
	t.Run("fat-fingered huge value is clamped", func(t *testing.T) {
		t.Setenv(envGitHubBodyMax, "999999999999")
		assert.Equal(t, maxGitHubMaxBytes, githubMaxBytes())
	})
	t.Run("reasonable custom value passes through", func(t *testing.T) {
		t.Setenv(envGitHubBodyMax, strconv.Itoa(4<<20))
		assert.Equal(t, 4<<20, githubMaxBytes())
	})
	t.Run("invalid / non-positive falls back to default", func(t *testing.T) {
		t.Setenv(envGitHubBodyMax, "-1")
		assert.Equal(t, defaultGitHubMaxBytes, githubMaxBytes())
		t.Setenv(envGitHubBodyMax, "garbage")
		assert.Equal(t, defaultGitHubMaxBytes, githubMaxBytes())
	})
}

func TestParseGitHubPush_CommitListTruncated(t *testing.T) {
	commits := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		commits = append(commits, fmt.Sprintf(`{"id":"sha%07d","message":"c%d","url":"u%d"}`, i, i, i))
	}
	body := fmt.Sprintf(`{"ref":"refs/heads/main","commits":[%s],"sender":{"login":"a"}}`, strings.Join(commits, ","))
	req, _, _ := parseGitHubPush(ghHeader("push"), []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "pushed 8 commit(s)")
	assert.Contains(t, req.Content, "…and 3 more", "only %d commits are listed", maxRenderedCommits)
	assert.NotContains(t, req.Content, "c7", "commits beyond the cap are not rendered")
}

func TestParseGitHubPush_PullRequest(t *testing.T) {
	tpl := `{
		"action": "%s",
		"pull_request": {"number": 12, "title": "Add feature", "html_url": "https://github.com/o/r/pull/12", "merged": %t},
		"repository": {"full_name": "o/r", "html_url": "https://github.com/o/r"},
		"sender": {"login": "carol"}
	}`
	t.Run("opened", func(t *testing.T) {
		req, _, _ := parseGitHubPush(ghHeader("pull_request"), []byte(fmt.Sprintf(tpl, "opened", false)))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**carol** opened pull request [#12 Add feature](https://github.com/o/r/pull/12)")
	})
	t.Run("closed merged renders as merged", func(t *testing.T) {
		req, _, _ := parseGitHubPush(ghHeader("pull_request"), []byte(fmt.Sprintf(tpl, "closed", true)))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "merged pull request")
	})
	t.Run("closed unmerged stays closed", func(t *testing.T) {
		req, _, _ := parseGitHubPush(ghHeader("pull_request"), []byte(fmt.Sprintf(tpl, "closed", false)))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "closed pull request")
	})
	t.Run("synchronize is skipped", func(t *testing.T) {
		req, skip, invalid := parseGitHubPush(ghHeader("pull_request"), []byte(fmt.Sprintf(tpl, "synchronize", false)))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
		assert.Empty(t, invalid)
	})
}

func TestParseGitHubPush_IssuesAndComments(t *testing.T) {
	t.Run("issue opened", func(t *testing.T) {
		body := `{"action":"opened","issue":{"number":3,"title":"Bug","html_url":"https://github.com/o/r/issues/3"},"sender":{"login":"dan"}}`
		req, _, _ := parseGitHubPush(ghHeader("issues"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**dan** opened issue [#3 Bug](https://github.com/o/r/issues/3)")
	})
	t.Run("issue labeled is skipped", func(t *testing.T) {
		body := `{"action":"labeled","issue":{"number":3,"title":"Bug"},"sender":{"login":"dan"}}`
		req, skip, _ := parseGitHubPush(ghHeader("issues"), []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
	})
	t.Run("comment created quotes a flattened snippet", func(t *testing.T) {
		body := `{"action":"created",
			"issue":{"number":3,"title":"Bug"},
			"comment":{"html_url":"https://github.com/o/r/issues/3#c1","body":"line one\nline two"},
			"sender":{"login":"eve"}}`
		req, _, _ := parseGitHubPush(ghHeader("issue_comment"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**eve** commented on [#3 Bug](https://github.com/o/r/issues/3#c1)")
		assert.Contains(t, req.Content, "> line one line two", "comment body is flattened to one line")
	})
	t.Run("comment edited is skipped", func(t *testing.T) {
		body := `{"action":"edited","issue":{"number":3},"comment":{"body":"x"},"sender":{"login":"eve"}}`
		req, skip, _ := parseGitHubPush(ghHeader("issue_comment"), []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
	})
}

func TestParseGitHubPush_Release(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		body := `{"action":"published","release":{"tag_name":"v2.0.0","name":"Big Release","html_url":"https://github.com/o/r/releases/v2.0.0"},"sender":{"login":"fred"}}`
		req, _, _ := parseGitHubPush(ghHeader("release"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**fred** published release [Big Release](https://github.com/o/r/releases/v2.0.0)")
	})
	t.Run("name falls back to tag", func(t *testing.T) {
		body := `{"action":"published","release":{"tag_name":"v2.0.0","html_url":"u"},"sender":{"login":"fred"}}`
		req, _, _ := parseGitHubPush(ghHeader("release"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "[v2.0.0](u)")
	})
	t.Run("created is skipped", func(t *testing.T) {
		body := `{"action":"created","release":{"tag_name":"v2.0.0"},"sender":{"login":"fred"}}`
		req, skip, _ := parseGitHubPush(ghHeader("release"), []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
	})
}

// 公开仓库可控的链接文本（PR/issue/release 标题）里的 `]`/`[` 必须转义，否则一个
// 形如 `evil]( ` 的标题会提前闭合 `[...]` 把后续 URL 暴露成可见文本甚至错位渲染
//（PR #330 review 跟进）。
func TestParseGitHubPush_LinkTextEscaped(t *testing.T) {
	// 标题里塞入会破坏链接结构的字符；断言渲染结果里这些字符已被反斜杠转义，
	// 且原始的 URL 边界 `](` 不会被标题内容提前引入。
	const evil = `pwn](http://evil) [x`
	cases := []struct {
		name  string
		event string
		body  string
		url   string
	}{
		{
			"pull_request title", "pull_request",
			fmt.Sprintf(`{"action":"opened","pull_request":{"number":1,"title":%q,"html_url":"https://safe/pr/1"},"sender":{"login":"a"}}`, evil),
			"https://safe/pr/1",
		},
		{
			"issue title", "issues",
			fmt.Sprintf(`{"action":"opened","issue":{"number":2,"title":%q,"html_url":"https://safe/i/2"},"sender":{"login":"a"}}`, evil),
			"https://safe/i/2",
		},
		{
			"release name", "release",
			fmt.Sprintf(`{"action":"published","release":{"tag_name":"v1","name":%q,"html_url":"https://safe/rel"},"sender":{"login":"a"}}`, evil),
			"https://safe/rel",
		},
		{
			"issue_comment issue title", "issue_comment",
			fmt.Sprintf(`{"action":"created","issue":{"number":3,"title":%q},"comment":{"html_url":"https://safe/c","body":"hi"},"sender":{"login":"a"}}`, evil),
			"https://safe/c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, skip, invalid := parseGitHubPush(ghHeader(tc.event), []byte(tc.body))
			require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
			assert.Contains(t, req.Content, `\]`, "closing bracket in title must be escaped")
			assert.Contains(t, req.Content, `\[`, "opening bracket in title must be escaped")
			// 真正的链接边界紧贴受控 URL，标题没能提前注入一个 `](`。
			assert.Contains(t, req.Content, "]("+tc.url+")")
			// 标题里紧跟 "pwn" 的 `]` 已被转义为 `\]`，不再是未转义的链接闭合。
			assert.NotContains(t, req.Content, "pwn](", "title's closing bracket must be escaped, not left raw")
		})
	}
}

// 平台事件里的超长字段被钳制，绝不让 GitHub 流量撞 413（调用方无法修短一个事件）。
func TestParseGitHubPush_ContentClipped(t *testing.T) {
	long := strings.Repeat("标", maxContentRunes()+500)
	body := fmt.Sprintf(`{"action":"opened","issue":{"number":1,"title":%q,"html_url":"u"},"sender":{"login":"g"}}`, long)
	req, _, _ := parseGitHubPush(ghHeader("issues"), []byte(body))
	require.NotNil(t, req)
	assert.LessOrEqual(t, len([]rune(req.Content)), maxContentRunes())
}
