package incomingwebhook

// GitHub 事件适配器（#297 Phase 3）。
//
// 路由：POST /v1/incoming-webhooks/:webhook_id/:token/github
// 在 GitHub 仓库 Settings → Webhooks 把 Payload URL 配成上述地址、Content type 选
// application/json 即可，无需任何中间转换层。
//
// 鉴权沿用 URL 内的 128-bit token（与 native 一致；经 #297 确认不强制 HMAC——
// X-Hub-Signature-256 校验留作后续可选项，参考 modules/webhook/hmac.go）。
//
// 渲染策略：按 X-GitHub-Event 把常用事件翻译成 markdown 文本（走 native 纯文本路径，
// 客户端按 markdown 渲染）。刻意只渲染「人关心的」动作子集——例如 pull_request 的
// synchronize（PR 分支每次 push 都触发）若也进群会刷屏。子集之外的事件 / 动作返回
// 200 并以 auditSkipped 落审计：GitHub 侧显示绿色投递成功（不会把 webhook 标红），
// 管理端 deliveries 里 reason=event 可见，两边都不糊弄。
//
// 所有 gh* 结构体只声明渲染需要的字段（白名单解析），其余 payload 字段一律忽略。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// 渲染的提交列表上限：push 事件单次最多带 20 个 commit（GitHub 截断），全列会刷屏。
const maxRenderedCommits = 5

// GitHub 事件 body 的字节上限。native 的 8KiB cap 约束的是调用方自己编写的 body，
// 而 GitHub 事件 JSON 由平台生成：真实 push / pull_request 事件（携带完整 repository
// 对象、提交列表）普遍在几十 KiB 量级，发送方无法修短，套用 8KiB 会把合法流量 413
// （PR #330 review 阻断项）。默认 1MiB：远高于现实事件（99% < 100KiB），仍是硬界——
// 且 body 读取发生在 token 鉴权 + per-webhook 5rps 限流之后，不构成放大面。
const (
	envGitHubBodyMax      = "DM_INCOMINGWEBHOOK_GITHUB_MAX_BYTES"
	defaultGitHubMaxBytes = 1 << 20 // 1MiB
)

func githubMaxBytes() int {
	if v := os.Getenv(envGitHubBodyMax); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultGitHubMaxBytes
}

type ghUser struct {
	Login string `json:"login"`
}

type ghRepo struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

type ghCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	URL     string `json:"url"`
}

type ghPushEvent struct {
	Ref        string     `json:"ref"`
	Created    bool       `json:"created"`
	Deleted    bool       `json:"deleted"`
	Forced     bool       `json:"forced"`
	Commits    []ghCommit `json:"commits"`
	Repository ghRepo     `json:"repository"`
	Sender     ghUser     `json:"sender"`
}

type ghPullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		Merged  bool   `json:"merged"`
	} `json:"pull_request"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

type ghIssuesEvent struct {
	Action string `json:"action"`
	Issue  struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

type ghIssueCommentEvent struct {
	Action string `json:"action"`
	Issue  struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Comment struct {
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	} `json:"comment"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

type ghReleaseEvent struct {
	Action  string `json:"action"`
	Release struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
	} `json:"release"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

// parseGitHubPush 把 GitHub webhook 事件翻译成 native 推送请求（pushAdapter.parse）。
func parseGitHubPush(header http.Header, body []byte) (*pushPayloadReq, string, string) {
	event := strings.TrimSpace(header.Get("X-GitHub-Event"))
	if event == "" {
		// 不带事件头的请求不可能来自 GitHub——按非法请求拒绝而非静默跳过，
		// 让误把 native 流量打到 /github 后缀的调用方立刻发现配置错误。
		return nil, "", "event"
	}

	var content string
	var err error
	switch event {
	case "ping":
		// GitHub 创建 webhook 时的连通性测试：200 即可，不发消息。
		return nil, "ping", ""
	case "push":
		content, err = renderGitHubPush(body)
	case "pull_request":
		content, err = renderGitHubPullRequest(body)
	case "issues":
		content, err = renderGitHubIssues(body)
	case "issue_comment":
		content, err = renderGitHubIssueComment(body)
	case "release":
		content, err = renderGitHubRelease(body)
	default:
		// 渲染子集之外的事件类型：通常只是 GitHub 侧订阅范围大于我们渲染的子集，
		// 调用方无需修复 → 200 + skipped（见文件头注释）。
		return nil, "event", ""
	}
	if err != nil {
		return nil, "", "json"
	}
	if content == "" {
		// 事件类型支持、但动作不在渲染子集内（synchronize / labeled / ...）：同上 skip。
		return nil, "event", ""
	}
	// 事件体里的标题 / 提交信息长度不受我们控制：钳到语义上限内，绝不让平台流量 413。
	return &pushPayloadReq{Content: clipRunes(content, maxContentRunes())}, "", ""
}

func renderGitHubPush(body []byte) (string, error) {
	var ev ghPushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	who := ghLogin(ev.Sender)
	ref := ghShortRef(ev.Ref)
	switch {
	case strings.HasPrefix(ev.Ref, "refs/tags/"):
		if ev.Deleted {
			return ghWithRepo(fmt.Sprintf("**%s** deleted tag `%s`", who, ref), ev.Repository), nil
		}
		return ghWithRepo(fmt.Sprintf("**%s** pushed tag `%s`", who, ref), ev.Repository), nil
	case ev.Deleted:
		return ghWithRepo(fmt.Sprintf("**%s** deleted branch `%s`", who, ref), ev.Repository), nil
	case ev.Created && len(ev.Commits) == 0:
		return ghWithRepo(fmt.Sprintf("**%s** created branch `%s`", who, ref), ev.Repository), nil
	case len(ev.Commits) == 0:
		// 非 create/delete 且无提交的退化 ref 更新（如 force-push 回相同内容）：渲染
		// "pushed 0 commit(s)" 只会制造噪音——返回空走 skip 路径（review 跟进，两位
		// reviewer 同时指出）。
		return "", nil
	}

	verb := "pushed"
	if ev.Forced {
		verb = "force-pushed"
	}
	var b strings.Builder
	b.WriteString(ghWithRepo(
		fmt.Sprintf("**%s** %s %d commit(s) to `%s`", who, verb, len(ev.Commits), ref),
		ev.Repository))
	for i, cm := range ev.Commits {
		if i == maxRenderedCommits {
			fmt.Fprintf(&b, "\n- …and %d more", len(ev.Commits)-maxRenderedCommits)
			break
		}
		fmt.Fprintf(&b, "\n- [`%s`](%s) %s", ghShortSHA(cm.ID), cm.URL, clipRunes(firstLine(cm.Message), 120))
	}
	return b.String(), nil
}

func renderGitHubPullRequest(body []byte) (string, error) {
	var ev ghPullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	var verb string
	switch ev.Action {
	case "opened", "reopened":
		verb = ev.Action
	case "ready_for_review":
		verb = "marked ready for review:"
	case "closed":
		verb = "closed"
		if ev.PullRequest.Merged {
			verb = "merged"
		}
	default:
		// synchronize / labeled / review_requested / ... 刷屏动作不渲染 → skip。
		return "", nil
	}
	return ghWithRepo(fmt.Sprintf("**%s** %s pull request [#%d %s](%s)",
		ghLogin(ev.Sender), verb, ev.PullRequest.Number,
		clipRunes(oneLine(ev.PullRequest.Title), 200), ev.PullRequest.HTMLURL),
		ev.Repository), nil
}

func renderGitHubIssues(body []byte) (string, error) {
	var ev ghIssuesEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	switch ev.Action {
	case "opened", "closed", "reopened":
	default:
		return "", nil
	}
	return ghWithRepo(fmt.Sprintf("**%s** %s issue [#%d %s](%s)",
		ghLogin(ev.Sender), ev.Action, ev.Issue.Number,
		clipRunes(oneLine(ev.Issue.Title), 200), ev.Issue.HTMLURL),
		ev.Repository), nil
}

func renderGitHubIssueComment(body []byte) (string, error) {
	var ev ghIssueCommentEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	if ev.Action != "created" {
		return "", nil
	}
	line := ghWithRepo(fmt.Sprintf("**%s** commented on [#%d %s](%s)",
		ghLogin(ev.Sender), ev.Issue.Number,
		clipRunes(oneLine(ev.Issue.Title), 200), ev.Comment.HTMLURL),
		ev.Repository)
	if snippet := clipRunes(oneLine(ev.Comment.Body), 300); snippet != "" {
		line += "\n> " + snippet
	}
	return line, nil
}

func renderGitHubRelease(body []byte) (string, error) {
	var ev ghReleaseEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	if ev.Action != "published" {
		return "", nil
	}
	title := ev.Release.Name
	if strings.TrimSpace(title) == "" {
		title = ev.Release.TagName
	}
	return ghWithRepo(fmt.Sprintf("**%s** published release [%s](%s)",
		ghLogin(ev.Sender), clipRunes(oneLine(title), 200), ev.Release.HTMLURL),
		ev.Repository), nil
}

// ghLogin 兜底空 sender（GitHub 偶发不带 sender，如某些 App 触发的事件）。
func ghLogin(u ghUser) string {
	if u.Login == "" {
		return "someone"
	}
	return u.Login
}

// ghWithRepo 给消息行追加 " · [repo](url)" 尾注；repo 信息缺失时原样返回。
func ghWithRepo(line string, r ghRepo) string {
	if r.FullName == "" {
		return line
	}
	if r.HTMLURL == "" {
		return line + " · " + r.FullName
	}
	return fmt.Sprintf("%s · [%s](%s)", line, r.FullName, r.HTMLURL)
}

// ghShortRef 把 refs/heads/main → main、refs/tags/v1.0 → v1.0。
func ghShortRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

// ghShortSHA 取提交短哈希（7 位，GitHub 惯例）。
func ghShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
