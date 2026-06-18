package incomingwebhook

// GitLab 事件适配器（#297 Phase 4）。
//
// 路由：POST /v1/incoming-webhooks/:webhook_id/:token/gitlab
// 在 GitLab 项目 Settings → Webhooks 把 URL 配成上述地址即可，无需中间转换层。
//
// 鉴权：除 URL 内的 128-bit token（与所有形态一致、由 handlePush 常量时间校验）外，
// GitLab 形态【额外】要求把项目 webhook 的「Secret token」设为同一个 token——GitLab
// 会以 X-Gitlab-Token 头回传，handlePush 经 verifyGitLabToken 常量时间比对。此闸在
// URL token 已验证之后，能到这里说明调用方已持有 webhook 真正的密钥，故 header 不匹配
// 是配置错误而非枚举探测（见 handlePush 注释 + #297 鉴权决定）。
//
// 渲染策略与 GitHub 适配器一致：按 X-Gitlab-Event 把常用事件翻译成 markdown 文本
// （走 native 纯文本路径），刻意只渲染「人关心的」动作子集，刷屏动作（MR update /
// pipeline running 等）返回 200 + skipped 落审计。所有 gl* 结构体只声明渲染需要的
// 字段（白名单解析），其余 payload 字段一律忽略。

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// 渲染的提交列表上限（与 GitHub 适配器一致：全列会刷屏）。
const maxRenderedGitLabCommits = 5

// glActorMax 是 actor display name 进 `**X**` 粗体前的 rune 钳长（与 multica 的
// shortFieldMax 同口径）。
const glActorMax = 64

// GitLab 事件 body 的字节上限。与 GitHub 同理：事件 JSON 由平台生成、普遍 >8KiB 且
// 发送方无法修短，套用 native 的 8KiB 会把合法流量 413。默认 1MiB，读取发生在 token
// 鉴权 + per-webhook 限流之后，不构成放大面。
const (
	envGitLabBodyMax      = "DM_INCOMINGWEBHOOK_GITLAB_MAX_BYTES"
	defaultGitLabMaxBytes = 1 << 20 // 1MiB
	// 与 github 同理（#297 Phase 3 review 跟进）：钳一个 25MiB 硬顶，避免一个被误填的
	// 巨大 env 把单请求 body 缓冲放大到危险量级——上限本就是防御性的。
	maxGitLabMaxBytes = 25 << 20 // 25MiB
)

func gitlabMaxBytes() int {
	n := defaultGitLabMaxBytes
	if v := os.Getenv(envGitLabBodyMax); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > maxGitLabMaxBytes {
		return maxGitLabMaxBytes
	}
	return n
}

// glIsZeroSHA 判断是否为 GitLab 的全零 SHA 占位（push 事件 before/after）：after 全零
// =删除 ref，before 全零=新建 ref（与 GitHub created/deleted 等价）。SHA1 仓库是 40 个
// 0、SHA256(object-format) 仓库是 64 个 0——按「非空且全 0」判定以兼容两种格式，否则
// SHA256 仓库的建/删 ref 通知会丢失（#423 review，yujiawei P2.3）。空串（字段缺省）不算。
func glIsZeroSHA(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}

// verifyGitLabToken 常量时间比对 X-Gitlab-Token 与 URL token。空头（未在 GitLab 配置
// Secret token）长度不等，ConstantTimeCompare 返回 0 → false。
func verifyGitLabToken(header http.Header, urlToken string) bool {
	got := header.Get("X-Gitlab-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(urlToken)) == 1
}

type glProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

type glCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	URL     string `json:"url"`
}

type glUser struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

type glPushEvent struct {
	Ref          string     `json:"ref"`
	Before       string     `json:"before"`
	After        string     `json:"after"`
	UserName     string     `json:"user_name"`
	UserUsername string     `json:"user_username"`
	Commits      []glCommit `json:"commits"`
	TotalCommits int        `json:"total_commits_count"`
	Project      glProject  `json:"project"`
}

type glMergeRequestEvent struct {
	User             glUser `json:"user"`
	ObjectAttributes struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Action string `json:"action"`
	} `json:"object_attributes"`
	Project glProject `json:"project"`
}

type glIssueEvent struct {
	User             glUser `json:"user"`
	ObjectAttributes struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Action string `json:"action"`
	} `json:"object_attributes"`
	Project glProject `json:"project"`
}

type glNoteEvent struct {
	User             glUser `json:"user"`
	ObjectAttributes struct {
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		URL          string `json:"url"`
		// System=true 是 GitLab 的「系统备注」（改标签/指派人/状态等自动生成的 note），
		// 与 GitHub 适配器只渲染人写的 issue_comment 一致，这类自动备注跳过免刷屏。
		System bool `json:"system"`
	} `json:"object_attributes"`
	MergeRequest struct {
		IID   int    `json:"iid"`
		Title string `json:"title"`
	} `json:"merge_request"`
	Issue struct {
		IID   int    `json:"iid"`
		Title string `json:"title"`
	} `json:"issue"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
	Project glProject `json:"project"`
}

type glPipelineEvent struct {
	ObjectAttributes struct {
		ID     int    `json:"id"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
	} `json:"object_attributes"`
	User    glUser    `json:"user"`
	Project glProject `json:"project"`
}

// parseGitLabPush 把 GitLab webhook 事件翻译成 native 推送请求（pushAdapter.parse）。
// X-Gitlab-Token 校验不在此处——它需要 URL token，由 handlePush 经 verifyGitLabToken
// 在鉴权闸里完成；本函数只负责按 X-Gitlab-Event 渲染。
func parseGitLabPush(header http.Header, body []byte) (*pushPayloadReq, string, string) {
	event := strings.TrimSpace(header.Get("X-Gitlab-Event"))
	if event == "" {
		// 不带事件头的请求不可能来自 GitLab——按非法请求拒绝而非静默跳过，让误把
		// native 流量打到 /gitlab 后缀的调用方立刻发现配置错误。与 github 一致用独立的
		// no_event，与「事件在渲染子集之外」的 200 skipped(reason=event) 区分开。
		return nil, "", "no_event"
	}

	var content string
	var err error
	switch event {
	case "Push Hook":
		content, err = renderGitLabPush(body)
	case "Tag Push Hook":
		content, err = renderGitLabTagPush(body)
	case "Merge Request Hook":
		content, err = renderGitLabMergeRequest(body)
	case "Issue Hook":
		content, err = renderGitLabIssue(body)
	case "Note Hook":
		content, err = renderGitLabNote(body)
	case "Pipeline Hook":
		content, err = renderGitLabPipeline(body)
	default:
		// 渲染子集之外的事件类型（Job Hook / Wiki Page Hook / ...）：通常只是订阅范围
		// 大于我们渲染的子集，调用方无需修复 → 200 + skipped。
		return nil, "event", ""
	}
	if err != nil {
		return nil, "", "json"
	}
	if content == "" {
		// 事件类型支持、但动作不在渲染子集内（MR update / pipeline running / ...）：skip。
		return nil, "event", ""
	}
	return &pushPayloadReq{Content: clipRunes(content, maxContentRunes())}, "", ""
}

func renderGitLabPush(body []byte) (string, error) {
	var ev glPushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	who := glActor(ev.UserUsername, ev.UserName)
	ref := glShortRef(ev.Ref)
	switch {
	case glIsZeroSHA(ev.After):
		return glWithRepo(fmt.Sprintf("**%s** deleted branch `%s`", who, ref), ev.Project), nil
	case glIsZeroSHA(ev.Before) && len(ev.Commits) == 0:
		return glWithRepo(fmt.Sprintf("**%s** created branch `%s`", who, ref), ev.Project), nil
	case len(ev.Commits) == 0:
		// 退化 ref 更新（无提交、非建/删）：渲染 "pushed 0 commit(s)" 只是噪音 → skip。
		return "", nil
	}

	// n = total_commits_count，但绝不小于实际渲染的 commits 数：total 缺省(0)时回退
	// len，且钳住 total < len 的畸形 payload，否则尾注会算出负数「…and -N more」
	//（#423 review，Jerry-Xin 🟡 hardening）。
	n := max(ev.TotalCommits, len(ev.Commits))
	var b strings.Builder
	b.WriteString(glWithRepo(
		fmt.Sprintf("**%s** pushed %d commit(s) to `%s`", who, n, ref), ev.Project))
	for i, cm := range ev.Commits {
		if i == maxRenderedGitLabCommits {
			// 用 n（total_commits_count）而非 len(ev.Commits)：GitLab 把 commits 数组
			// 截断到约 20 条，一次 100 提交的 push 里 len=20 但 total=100，用 len 会渲染
			// 自相矛盾的「pushed 100 commit(s) … and 15 more」，应是「…and 95 more」
			//（#423 review，yujiawei P1）。
			fmt.Fprintf(&b, "\n- …and %d more", n-maxRenderedGitLabCommits)
			break
		}
		fmt.Fprintf(&b, "\n- [`%s`](%s) %s", glShortSHA(cm.ID), cm.URL, clipRunes(firstLine(cm.Message), 120))
	}
	return b.String(), nil
}

func renderGitLabTagPush(body []byte) (string, error) {
	var ev glPushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	who := glActor(ev.UserUsername, ev.UserName)
	tag := glShortRef(ev.Ref)
	if glIsZeroSHA(ev.After) {
		return glWithRepo(fmt.Sprintf("**%s** deleted tag `%s`", who, tag), ev.Project), nil
	}
	return glWithRepo(fmt.Sprintf("**%s** pushed tag `%s`", who, tag), ev.Project), nil
}

func renderGitLabMergeRequest(body []byte) (string, error) {
	var ev glMergeRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	verb := glActionVerb(ev.ObjectAttributes.Action)
	if verb == "" {
		// update / approved / unapproved / ... 刷屏动作不渲染 → skip。
		return "", nil
	}
	return glWithRepo(fmt.Sprintf("**%s** %s merge request [!%d %s](%s)",
		glActor(ev.User.Username, ev.User.Name), verb, ev.ObjectAttributes.IID,
		mdLinkText(ev.ObjectAttributes.Title, 200), ev.ObjectAttributes.URL),
		ev.Project), nil
}

func renderGitLabIssue(body []byte) (string, error) {
	var ev glIssueEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	verb := glActionVerb(ev.ObjectAttributes.Action)
	if verb == "" {
		return "", nil
	}
	return glWithRepo(fmt.Sprintf("**%s** %s issue [#%d %s](%s)",
		glActor(ev.User.Username, ev.User.Name), verb, ev.ObjectAttributes.IID,
		mdLinkText(ev.ObjectAttributes.Title, 200), ev.ObjectAttributes.URL),
		ev.Project), nil
}

func renderGitLabNote(body []byte) (string, error) {
	var ev glNoteEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	if ev.ObjectAttributes.System {
		// 系统备注（改标签/指派/状态等自动生成）：与 GitHub 只渲染人写评论一致，skip。
		return "", nil
	}
	who := glActor(ev.User.Username, ev.User.Name)
	url := ev.ObjectAttributes.URL
	var target string
	switch ev.ObjectAttributes.NoteableType {
	case "MergeRequest":
		target = fmt.Sprintf("[!%d %s](%s)", ev.MergeRequest.IID,
			mdLinkText(ev.MergeRequest.Title, 200), url)
	case "Issue":
		target = fmt.Sprintf("[#%d %s](%s)", ev.Issue.IID,
			mdLinkText(ev.Issue.Title, 200), url)
	case "Commit":
		target = fmt.Sprintf("[commit `%s`](%s)", glShortSHA(ev.Commit.ID), url)
	default:
		// Snippet 等少见目标：仍渲染一条通用评论，附链接。
		target = fmt.Sprintf("[a comment](%s)", url)
	}
	line := glWithRepo(fmt.Sprintf("**%s** commented on %s", who, target), ev.Project)
	if snippet := clipRunes(oneLine(ev.ObjectAttributes.Note), 300); snippet != "" {
		line += "\n> " + snippet
	}
	return line, nil
}

func renderGitLabPipeline(body []byte) (string, error) {
	var ev glPipelineEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", err
	}
	// 只渲染终态：running / pending / created / manual / skipped 都会刷屏 → skip。
	switch ev.ObjectAttributes.Status {
	case "success", "failed", "canceled":
	default:
		return "", nil
	}
	// Pipeline 是唯一自拼 URL 的事件（MR/Issue/Note 直接用 object_attributes.url 绝对
	// 地址）。project.web_url 缺失时（白名单解析不保证字段必到）退化为不带链接的纯文本，
	// 避免渲染出 [#99](/-/pipelines/99) 这种不可点击的相对路径（#423 review，lml2468）。
	var line string
	if ev.Project.WebURL != "" {
		line = fmt.Sprintf("Pipeline [#%d](%s/-/pipelines/%d) %s on `%s`",
			ev.ObjectAttributes.ID, ev.Project.WebURL, ev.ObjectAttributes.ID,
			ev.ObjectAttributes.Status, glShortRef(ev.ObjectAttributes.Ref))
	} else {
		line = fmt.Sprintf("Pipeline #%d %s on `%s`",
			ev.ObjectAttributes.ID, ev.ObjectAttributes.Status, glShortRef(ev.ObjectAttributes.Ref))
	}
	return glWithRepo(line, ev.Project), nil
}

// glActor 优先用 username（GitLab 用户名字符集受限：[a-zA-Z0-9_.-]，进 `**X**` 粗体
// 无注入面），回退 display name，再兜底 "someone"。display name 是自由文本，进粗体上
// 下文必须经 mdInertText 转义（`*`/`[`/`]`/`<` 等），否则一个名为
// `**evil** [x](http://attacker)` 的用户能往群消息里注入粗体+可点击链接——与
// adapter_multica.go 对 actor/identifier 的处理同口径（#423 review，Jerry-Xin/mochashanyao）。
func glActor(username, name string) string {
	if username != "" {
		return username
	}
	if name != "" {
		return mdInertText(name, glActorMax)
	}
	return "someone"
}

// glActionVerb 把 GitLab 的 MR/Issue object_attributes.action 映射为渲染动词；
// 返回空表示该动作在渲染子集之外（调用方无需修复，走 skip）。
func glActionVerb(action string) string {
	switch action {
	case "open":
		return "opened"
	case "close":
		return "closed"
	case "reopen":
		return "reopened"
	case "merge":
		return "merged"
	default:
		return ""
	}
}

// glWithRepo 给消息行追加 " · [namespace/project](url)" 尾注；项目信息缺失时原样返回。
// path_with_namespace 进链接文本 / 纯文本尾注都过 mdInertText 转义——GitLab 项目路径
// 字符集虽受限，但与 #421 对 ghWithRepo FullName 的处理同口径、消除注入面（#423
// review，lml2468 should-fix）。
func glWithRepo(line string, p glProject) string {
	if p.PathWithNamespace == "" {
		return line
	}
	name := mdInertText(p.PathWithNamespace, 200)
	if p.WebURL == "" {
		return line + " · " + name
	}
	return fmt.Sprintf("%s · [%s](%s)", line, name, p.WebURL)
}

// glShortRef 把 refs/heads/main → main、refs/tags/v1.0 → v1.0。
func glShortRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

// glShortSHA 取提交短哈希（8 位，GitLab 惯例）。
func glShortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
