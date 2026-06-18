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

// GitLab 适配器纯翻译单测（无 DB/Redis/IM 依赖）。fixture 取自 GitLab webhook 文档的
// 字段子集——gl* 结构体本就是白名单解析，多余字段与缺省字段都不影响结果。

func glHeader(event string) http.Header {
	h := http.Header{}
	if event != "" {
		h.Set("X-Gitlab-Event", event)
	}
	return h
}

func TestVerifyGitLabToken(t *testing.T) {
	h := http.Header{}
	h.Set("X-Gitlab-Token", "secret-token-123")
	assert.True(t, verifyGitLabToken(h, "secret-token-123"), "matching token passes")
	assert.False(t, verifyGitLabToken(h, "secret-token-999"), "mismatched token fails")
	assert.False(t, verifyGitLabToken(http.Header{}, "secret-token-123"), "missing header fails")
	assert.False(t, verifyGitLabToken(h, ""), "empty url token fails")
}

func TestParseGitLabPush_HeaderGate(t *testing.T) {
	t.Run("missing event header is invalid", func(t *testing.T) {
		req, skip, invalid := parseGitLabPush(http.Header{}, []byte(`{}`))
		assert.Nil(t, req)
		assert.Empty(t, skip)
		// 缺事件头是配置错误 → 独立 no_event（与 github 一致），与渲染子集之外的
		// 200 skipped(event) 区分。
		assert.Equal(t, "no_event", invalid)
	})
	t.Run("unsupported event is skipped", func(t *testing.T) {
		req, skip, invalid := parseGitLabPush(glHeader("Wiki Page Hook"), []byte(`{}`))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
		assert.Empty(t, invalid)
	})
	t.Run("malformed body is invalid json", func(t *testing.T) {
		req, skip, invalid := parseGitLabPush(glHeader("Push Hook"), []byte(`{not json`))
		assert.Nil(t, req)
		assert.Empty(t, skip)
		assert.Equal(t, "json", invalid)
	})
}

func TestParseGitLabPush_PushEvent(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"before": "1111111111111111111111111111111111111111",
		"after": "2222222222222222222222222222222222222222",
		"user_username": "alice",
		"user_name": "Alice Liddell",
		"total_commits_count": 2,
		"commits": [
			{"id": "aaaabbbbccccdddd1111", "message": "feat: first\n\nbody", "url": "https://gitlab.com/o/r/-/commit/aaaabbbb"},
			{"id": "1111222233334444aaaa", "message": "fix: second", "url": "https://gitlab.com/o/r/-/commit/11112222"}
		],
		"project": {"path_with_namespace": "octo/repo", "web_url": "https://gitlab.com/octo/repo"}
	}`)
	req, skip, invalid := parseGitLabPush(glHeader("Push Hook"), body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Contains(t, req.Content, "**alice** pushed 2 commit(s) to `main`")
	assert.Contains(t, req.Content, "[octo/repo](https://gitlab.com/octo/repo)")
	assert.Contains(t, req.Content, "[`aaaabbbb`](https://gitlab.com/o/r/-/commit/aaaabbbb) feat: first")
	assert.NotContains(t, req.Content, "body", "only the first line of a commit message is rendered")
	assert.Empty(t, req.MsgType, "adapters emit the plain-text path")
}

func TestParseGitLabPush_PushVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"branch create", `{"ref":"refs/heads/dev","before":"0000000000000000000000000000000000000000","after":"abc","commits":[],"user_username":"bob"}`,
			"**bob** created branch `dev`"},
		{"branch delete", `{"ref":"refs/heads/dev","after":"0000000000000000000000000000000000000000","user_username":"bob"}`,
			"**bob** deleted branch `dev`"},
		{"display-name fallback", `{"ref":"refs/heads/main","commits":[{"id":"abcabcabc","message":"m","url":"u"}],"total_commits_count":1,"user_name":"Bob Builder"}`,
			"**Bob Builder** pushed 1 commit(s)"},
		{"missing user falls back", `{"ref":"refs/heads/main","commits":[{"id":"abc","message":"m","url":"u"}],"total_commits_count":1}`,
			"**someone** pushed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, skip, invalid := parseGitLabPush(glHeader("Push Hook"), []byte(tc.body))
			require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
			assert.Contains(t, req.Content, tc.want)
		})
	}
}

// 退化 ref 更新（无提交、非建/删）不渲染 "pushed 0 commit(s)"，走 skip。
func TestParseGitLabPush_NoCommitRefUpdateSkipped(t *testing.T) {
	body := `{"ref":"refs/heads/main","before":"aaa","after":"bbb","commits":[],"user_username":"bob"}`
	req, skip, invalid := parseGitLabPush(glHeader("Push Hook"), []byte(body))
	assert.Nil(t, req)
	assert.Equal(t, "event", skip)
	assert.Empty(t, invalid)
}

func TestParseGitLabPush_CommitListTruncated(t *testing.T) {
	commits := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		commits = append(commits, fmt.Sprintf(`{"id":"sha%016d","message":"c%d","url":"u%d"}`, i, i, i))
	}
	body := fmt.Sprintf(`{"ref":"refs/heads/main","total_commits_count":8,"commits":[%s],"user_username":"a"}`, strings.Join(commits, ","))
	req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "pushed 8 commit(s)")
	assert.Contains(t, req.Content, "…and 3 more", "only %d commits are listed", maxRenderedGitLabCommits)
	assert.NotContains(t, req.Content, "c7", "commits beyond the cap are not rendered")
}

// GitLab 把 commits 数组截断到 ~20，total_commits_count 才是真实总数。溢出尾注必须用
// total（与 header 一致），不能用截断后的 len(commits)（#423 review，yujiawei P1）。
func TestParseGitLabPush_CommitOverflowUsesTotalCount(t *testing.T) {
	commits := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		commits = append(commits, fmt.Sprintf(`{"id":"sha%016d","message":"c%d","url":"u%d"}`, i, i, i))
	}
	body := fmt.Sprintf(`{"ref":"refs/heads/main","total_commits_count":100,"commits":[%s],"user_username":"a"}`, strings.Join(commits, ","))
	req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "pushed 100 commit(s)")
	assert.Contains(t, req.Content, "…and 95 more", "overflow must agree with total_commits_count (100-5), not the truncated array length")
	assert.NotContains(t, req.Content, "…and 15 more", "must not use len(commits) for the overflow count")
}

// 畸形 payload：total_commits_count < len(commits)。n 钳到 len，绝不渲染负数尾注
// （#423 review，Jerry-Xin 🟡 hardening）。
func TestParseGitLabPush_CommitCountClampedWhenTotalBelowLen(t *testing.T) {
	commits := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		commits = append(commits, fmt.Sprintf(`{"id":"sha%016d","message":"c%d","url":"u%d"}`, i, i, i))
	}
	body := fmt.Sprintf(`{"ref":"refs/heads/main","total_commits_count":3,"commits":[%s],"user_username":"a"}`, strings.Join(commits, ","))
	req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "pushed 8 commit(s)", "header clamps to the real commit count")
	assert.Contains(t, req.Content, "…and 3 more")
	assert.NotContains(t, req.Content, "and -", "never render a negative overflow count")
}

// actor 的 display-name 回退是自由文本，进 `**X**` 粗体必须转义，杜绝粗体/链接注入
// （#423 review，Jerry-Xin/mochashanyao）。
func TestParseGitLabPush_ActorNameEscaped(t *testing.T) {
	// username 缺失 → 回退 user_name；该名是注入串。
	body := `{"ref":"refs/heads/main","total_commits_count":1,"commits":[{"id":"abc","message":"m","url":"u"}],"user_name":"**evil** [x](http://attacker)"}`
	req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, `\*\*evil\*\*`, "actor bold markers must be escaped")
	assert.Contains(t, req.Content, `\[x\]`, "actor brackets must be escaped so no clickable link is injected")
}

// SHA256 object-format 仓库的全零占位是 64 个 0（SHA1 是 40）。建/删 ref 必须两种都认
// （#423 review，yujiawei P2.3）。
func TestParseGitLabPush_SHA256ZeroSentinel(t *testing.T) {
	zero64 := strings.Repeat("0", 64)
	t.Run("sha256 branch delete", func(t *testing.T) {
		body := fmt.Sprintf(`{"ref":"refs/heads/dev","after":%q,"user_username":"bob"}`, zero64)
		req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**bob** deleted branch `dev`")
	})
	t.Run("sha256 branch create", func(t *testing.T) {
		body := fmt.Sprintf(`{"ref":"refs/heads/dev","before":%q,"after":"abc","commits":[],"user_username":"bob"}`, zero64)
		req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**bob** created branch `dev`")
	})
}

// path_with_namespace 是外部输入，进尾注（链接文本/纯文本）须经 mdInertText 转义，
// 消除注入面（#423 review，lml2468 should-fix；与 #421 ghWithRepo 同口径）。
func TestParseGitLabPush_ProjectNameEscaped(t *testing.T) {
	body := `{"ref":"refs/heads/main","total_commits_count":1,"commits":[{"id":"abc","message":"m","url":"u"}],"user_username":"a","project":{"path_with_namespace":"o/[x]","web_url":"https://safe/p"}}`
	req, _, _ := parseGitLabPush(glHeader("Push Hook"), []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, `\[x\]`, "project name brackets must be escaped")
	assert.Contains(t, req.Content, "](https://safe/p)", "real link boundary preserved")
}

func TestParseGitLabPush_TagPush(t *testing.T) {
	t.Run("push tag", func(t *testing.T) {
		body := `{"ref":"refs/tags/v1.2.0","after":"abc","user_username":"bob","project":{"path_with_namespace":"o/r"}}`
		req, _, _ := parseGitLabPush(glHeader("Tag Push Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**bob** pushed tag `v1.2.0`")
	})
	t.Run("delete tag", func(t *testing.T) {
		body := `{"ref":"refs/tags/v1.2.0","after":"0000000000000000000000000000000000000000","user_username":"bob"}`
		req, _, _ := parseGitLabPush(glHeader("Tag Push Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**bob** deleted tag `v1.2.0`")
	})
}

func TestParseGitLabPush_MergeRequest(t *testing.T) {
	tpl := `{
		"user": {"username": "carol"},
		"object_attributes": {"iid": 12, "title": "Add feature", "url": "https://gitlab.com/o/r/-/merge_requests/12", "action": "%s"},
		"project": {"path_with_namespace": "o/r", "web_url": "https://gitlab.com/o/r"}
	}`
	t.Run("open", func(t *testing.T) {
		req, _, _ := parseGitLabPush(glHeader("Merge Request Hook"), []byte(fmt.Sprintf(tpl, "open")))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**carol** opened merge request [!12 Add feature](https://gitlab.com/o/r/-/merge_requests/12)")
	})
	t.Run("merge", func(t *testing.T) {
		req, _, _ := parseGitLabPush(glHeader("Merge Request Hook"), []byte(fmt.Sprintf(tpl, "merge")))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "merged merge request")
	})
	t.Run("update is skipped", func(t *testing.T) {
		req, skip, invalid := parseGitLabPush(glHeader("Merge Request Hook"), []byte(fmt.Sprintf(tpl, "update")))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
		assert.Empty(t, invalid)
	})
}

func TestParseGitLabPush_Issue(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		body := `{"user":{"username":"dan"},"object_attributes":{"iid":3,"title":"Bug","url":"https://gitlab.com/o/r/-/issues/3","action":"open"}}`
		req, _, _ := parseGitLabPush(glHeader("Issue Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**dan** opened issue [#3 Bug](https://gitlab.com/o/r/-/issues/3)")
	})
	t.Run("update is skipped", func(t *testing.T) {
		body := `{"user":{"username":"dan"},"object_attributes":{"iid":3,"title":"Bug","action":"update"}}`
		req, skip, _ := parseGitLabPush(glHeader("Issue Hook"), []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
	})
}

func TestParseGitLabPush_Note(t *testing.T) {
	t.Run("merge request comment", func(t *testing.T) {
		body := `{"user":{"username":"eve"},
			"object_attributes":{"note":"line one\nline two","noteable_type":"MergeRequest","url":"https://gitlab.com/o/r/-/merge_requests/12#note_1"},
			"merge_request":{"iid":12,"title":"Add feature"}}`
		req, _, _ := parseGitLabPush(glHeader("Note Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**eve** commented on [!12 Add feature](https://gitlab.com/o/r/-/merge_requests/12#note_1)")
		assert.Contains(t, req.Content, "> line one line two", "note body is flattened to one line")
	})
	t.Run("issue comment", func(t *testing.T) {
		body := `{"user":{"username":"eve"},
			"object_attributes":{"note":"hi","noteable_type":"Issue","url":"https://gitlab.com/o/r/-/issues/3#note_2"},
			"issue":{"iid":3,"title":"Bug"}}`
		req, _, _ := parseGitLabPush(glHeader("Note Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**eve** commented on [#3 Bug](https://gitlab.com/o/r/-/issues/3#note_2)")
	})
	t.Run("system note is skipped", func(t *testing.T) {
		// GitLab 把改标签/指派等自动生成的「系统备注」也走 Note Hook 投递（system=true），
		// 渲染它们会刷屏——与 GitHub 只渲染人写评论一致，跳过。
		body := `{"user":{"username":"eve"},
			"object_attributes":{"note":"changed the description","noteable_type":"Issue","system":true},
			"issue":{"iid":3,"title":"Bug"}}`
		req, skip, invalid := parseGitLabPush(glHeader("Note Hook"), []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
		assert.Empty(t, invalid)
	})
}

func TestParseGitLabPush_Pipeline(t *testing.T) {
	tpl := `{"object_attributes":{"id":99,"ref":"main","status":"%s"},"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`
	t.Run("failed is rendered", func(t *testing.T) {
		req, _, _ := parseGitLabPush(glHeader("Pipeline Hook"), []byte(fmt.Sprintf(tpl, "failed")))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "Pipeline [#99](https://gitlab.com/o/r/-/pipelines/99) failed on `main`")
	})
	t.Run("success is rendered", func(t *testing.T) {
		req, _, _ := parseGitLabPush(glHeader("Pipeline Hook"), []byte(fmt.Sprintf(tpl, "success")))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "Pipeline [#99](https://gitlab.com/o/r/-/pipelines/99) success on `main`")
	})
	t.Run("running is skipped", func(t *testing.T) {
		req, skip, _ := parseGitLabPush(glHeader("Pipeline Hook"), []byte(fmt.Sprintf(tpl, "running")))
		assert.Nil(t, req)
		assert.Equal(t, "event", skip)
	})
	t.Run("missing web_url degrades to plain text (no broken relative link)", func(t *testing.T) {
		body := `{"object_attributes":{"id":99,"ref":"main","status":"failed"},"project":{"path_with_namespace":"o/r"}}`
		req, _, _ := parseGitLabPush(glHeader("Pipeline Hook"), []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "Pipeline #99 failed on `main`")
		assert.NotContains(t, req.Content, "](/-/pipelines", "must not emit a relative-path link when web_url is absent")
		assert.NotContains(t, req.Content, "[#99]", "no markdown link without an absolute base url")
	})
}

// 平台事件里的超长字段被钳制，绝不让 GitLab 流量撞 413（调用方无法修短一个事件）。
func TestParseGitLabPush_ContentClipped(t *testing.T) {
	long := strings.Repeat("标", maxContentRunes()+500)
	body := fmt.Sprintf(`{"user":{"username":"g"},"object_attributes":{"iid":1,"title":%q,"url":"u","action":"open"}}`, long)
	req, _, _ := parseGitLabPush(glHeader("Issue Hook"), []byte(body))
	require.NotNil(t, req)
	assert.LessOrEqual(t, len([]rune(req.Content)), maxContentRunes())
}

// GitLab 事件 body 是平台生成的，其上限必须宽于 native 的调用方编写上限。两个 cap 的
// env 都清空钉到默认值，结果不依赖宿主机环境变量。
func TestGitLabMaxBytes_ExceedsNativeCap(t *testing.T) {
	t.Setenv(envBodyMax, "")
	t.Setenv(envGitLabBodyMax, "")
	assert.Greater(t, gitlabMaxBytes(), maxBytes())
	assert.Equal(t, defaultGitLabMaxBytes, gitlabMaxBytes())
}

// env 被手误填成天文数字时，gitlabMaxBytes 钳到 25MiB 硬顶（与 github 一致）。
func TestGitLabMaxBytes_ClampsHugeEnv(t *testing.T) {
	t.Run("fat-fingered huge value is clamped", func(t *testing.T) {
		t.Setenv(envGitLabBodyMax, "999999999999")
		assert.Equal(t, maxGitLabMaxBytes, gitlabMaxBytes())
	})
	t.Run("reasonable custom value passes through", func(t *testing.T) {
		t.Setenv(envGitLabBodyMax, strconv.Itoa(4<<20))
		assert.Equal(t, 4<<20, gitlabMaxBytes())
	})
	t.Run("invalid / non-positive falls back to default", func(t *testing.T) {
		t.Setenv(envGitLabBodyMax, "-1")
		assert.Equal(t, defaultGitLabMaxBytes, gitlabMaxBytes())
	})
}

// 公开仓库可控的链接文本（MR/Issue 标题）里的 `]`/`[` 必须转义，避免破坏渲染出的
// 链接（与 github 适配器同一处理）。
func TestParseGitLabPush_LinkTextEscaped(t *testing.T) {
	body := `{"user":{"username":"a"},"object_attributes":{"iid":1,"title":"pwn](http://evil) [x","url":"https://safe/mr/1","action":"open"}}`
	req, skip, invalid := parseGitLabPush(glHeader("Merge Request Hook"), []byte(body))
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Contains(t, req.Content, `\]`, "closing bracket in title must be escaped")
	assert.Contains(t, req.Content, `\[`, "opening bracket in title must be escaped")
	assert.Contains(t, req.Content, "](https://safe/mr/1)")
	assert.NotContains(t, req.Content, "pwn](", "title's closing bracket must be escaped, not left raw")
}
