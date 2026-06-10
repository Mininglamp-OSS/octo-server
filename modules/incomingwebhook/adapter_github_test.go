package incomingwebhook

import (
	"fmt"
	"net/http"
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
		assert.Equal(t, "event", invalid)
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
// native 的调用方编写上限——钉住 review 阻断项的修复不被回退。
func TestGitHubMaxBytes_ExceedsNativeCap(t *testing.T) {
	assert.Greater(t, githubMaxBytes(), maxBytes())
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

// 平台事件里的超长字段被钳制，绝不让 GitHub 流量撞 413（调用方无法修短一个事件）。
func TestParseGitHubPush_ContentClipped(t *testing.T) {
	long := strings.Repeat("标", maxContentRunes()+500)
	body := fmt.Sprintf(`{"action":"opened","issue":{"number":1,"title":%q,"html_url":"u"},"sender":{"login":"g"}}`, long)
	req, _, _ := parseGitHubPush(ghHeader("issues"), []byte(body))
	require.NotNil(t, req)
	assert.LessOrEqual(t, len([]rune(req.Content)), maxContentRunes())
}
