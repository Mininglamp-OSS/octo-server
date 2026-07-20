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
// 客户端按 markdown 渲染）。pull_request/issues/issue_comment/release 的所有 action
// 均会渲染（产品决定：不按「是否刷屏」过滤，与 GitLab 适配器同一决定——旧版本只渲染
// open/close/reopen/merge 等子集，现已放开）；仍在渲染子集之外的只有事件【类型】本身
// （watch / star 等未实现的事件类型）与畸形 payload（缺 action 字段）。子集之外的
// 事件返回 200 并以 auditSkipped 落审计：GitHub 侧显示绿色投递成功（不会把 webhook
// 标红），管理端 deliveries 里 reason=event 可见，两边都不糊弄。
//
// 所有 gh* 结构体只声明渲染需要的字段（白名单解析），其余 payload 字段一律忽略。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
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
	// GitHub 自身把单次 webhook delivery 的 payload 上限定在 25MB，超过这个值的配置
	// 必然是手误（多打几个 0）。给 env 钳一个硬顶，避免一个被误填的巨大值把单请求的
	// body 缓冲放大到危险量级——上限本就是防御性的，不该因配置失误反而失去防御
	//（PR #330 review 跟进）。
	maxGitHubMaxBytes = 25 << 20 // 25MiB
)

func githubMaxBytes() int {
	n := defaultGitHubMaxBytes
	if v := os.Getenv(envGitHubBodyMax); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > maxGitHubMaxBytes {
		return maxGitHubMaxBytes
	}
	return n
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
	Ref     string     `json:"ref"`
	Created bool       `json:"created"`
	Deleted bool       `json:"deleted"`
	Forced  bool       `json:"forced"`
	Commits []ghCommit `json:"commits"`
	// Compare is GitHub's canonical diff URL for the push (card "View" target;
	// unused by the text renderer). Falls back to the repo URL when absent.
	Compare    string `json:"compare"`
	Repository ghRepo `json:"repository"`
	Sender     ghUser `json:"sender"`
}

// ghLabel is a GitHub label object (the `labels[]` array GitHub attaches to
// pull_request/issues event payloads, nested under pull_request/issue rather than
// top-level like GitLab's). Only Name is rendered.
type ghLabel struct {
	Name string `json:"name"`
}

type ghPullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		Merged  bool   `json:"merged"`
		// Base/Head feed the card-only Target/Source FactSet rows (text path
		// unchanged, same "card-only" convention as the GitLab MR card).
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		// Labels feeds the card-only Labels FactSet row;缺省即不展示该行。
		Labels []ghLabel `json:"labels"`
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
		// Labels feeds the card-only Labels FactSet row;缺省即不展示该行。
		Labels []ghLabel `json:"labels"`
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
		// 让误把 native 流量打到 /github 后缀的调用方立刻发现配置错误。用独立的
		// no_event 与「事件在渲染子集之外」的 200 skipped(reason=event) 区分开：
		// 二者语义不同（前者是配置错误，后者是正常但不渲染），deliveries 里只看
		// reason 即可分辨，不必再去比对 status/http_status（PR #330 review 跟进）。
		return nil, "", "no_event"
	}

	if event == "ping" {
		// GitHub 创建 webhook 时的连通性测试：200 即可，不发消息。
		return nil, "ping", ""
	}
	// card-message webhook-cardmsg-adapter：开关开时把同一事件渲成 InteractiveCard(=17)，
	// 关闭（或卡片自校验失败）时 vcsPushReq 降级回 markdown 文本路径（文本渲染器输出不变，
	// flag-off 字节与历史一致）。body 每事件【只反序列化一次】，文本与卡片共用同一 *ev。
	// 事件体里的标题 / 提交信息长度不受我们控制：文本路径钳到语义上限内（vcsPushReq 内）。
	wantCard := cardmsg.Enabled()
	lang := ""
	if wantCard {
		lang = i18n.OutboundLanguage(context.Background())
	}
	var content string
	var card map[string]interface{}
	switch event {
	case "push":
		var ev ghPushEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitHubPush(&ev)
		if content != "" && wantCard {
			card = buildGitHubPushCard(&ev, lang)
		}
	case "pull_request":
		var ev ghPullRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitHubPullRequest(&ev)
		if content != "" && wantCard {
			card = buildGitHubPullRequestCard(&ev, lang)
		}
	case "issues":
		var ev ghIssuesEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitHubIssues(&ev)
		if content != "" && wantCard {
			card = buildGitHubIssuesCard(&ev, lang)
		}
	case "issue_comment":
		var ev ghIssueCommentEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitHubIssueComment(&ev)
		if content != "" && wantCard {
			card = buildGitHubIssueCommentCard(&ev, lang)
		}
	case "release":
		var ev ghReleaseEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitHubRelease(&ev)
		if content != "" && wantCard {
			card = buildGitHubReleaseCard(&ev, lang)
		}
	default:
		// 渲染子集之外的事件类型：通常只是 GitHub 侧订阅范围大于我们渲染的子集，
		// 调用方无需修复 → 200 + skipped（见文件头注释）。
		return nil, "event", ""
	}
	if content == "" {
		// 事件类型支持、但动作不在渲染子集内（synchronize / labeled / ...）：同上 skip。
		return nil, "event", ""
	}
	return vcsPushReq(content, card), "", ""
}

// renderGitHubPush renders the text-path markdown for a push event. Returns "" for a
// degenerate ref update outside the rendered subset (caller treats "" as skip). The
// body is unmarshalled once by the caller; this operates on the shared *ev.
func renderGitHubPush(ev *ghPushEvent) string {
	who := ghLogin(ev.Sender)
	ref := ghShortRef(ev.Ref)
	switch {
	case strings.HasPrefix(ev.Ref, "refs/tags/"):
		if ev.Deleted {
			return ghWithRepo(fmt.Sprintf("**%s** deleted tag `%s`", who, ref), ev.Repository)
		}
		return ghWithRepo(fmt.Sprintf("**%s** pushed tag `%s`", who, ref), ev.Repository)
	case ev.Deleted:
		return ghWithRepo(fmt.Sprintf("**%s** deleted branch `%s`", who, ref), ev.Repository)
	case ev.Created && len(ev.Commits) == 0:
		return ghWithRepo(fmt.Sprintf("**%s** created branch `%s`", who, ref), ev.Repository)
	case len(ev.Commits) == 0:
		// 非 create/delete 且无提交的退化 ref 更新（如 force-push 回相同内容）：渲染
		// "pushed 0 commit(s)" 只会制造噪音——返回空走 skip 路径（review 跟进，两位
		// reviewer 同时指出）。
		return ""
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
	return b.String()
}

func renderGitHubPullRequest(ev *ghPullRequestEvent) string {
	verb := ghPRVerb(ev.Action, ev.PullRequest.Merged)
	if verb == "" {
		// 缺 action 字段的畸形 payload：没有可渲染的动作 → skip（唯一保留的过滤）。
		return ""
	}
	pr := fmt.Sprintf("pull request [#%d %s](%s)", ev.PullRequest.Number,
		mdLinkText(ev.PullRequest.Title, 200), ev.PullRequest.HTMLURL)
	// ready_for_review/converted_to_draft 是完整谓语短语，不是能直接接宾语的及物动词，
	// 套用通用的 "VERB pull request [#N]" 模板会语序错乱（"marked ready for review
	// pull request #N"）。这两个 action 是已知字面量分支，宾语单独摆在短语中间。
	switch ev.Action {
	case "ready_for_review":
		return ghWithRepo(fmt.Sprintf("**%s** marked %s ready for review", ghLogin(ev.Sender), pr), ev.Repository)
	case "converted_to_draft":
		return ghWithRepo(fmt.Sprintf("**%s** converted %s to draft", ghLogin(ev.Sender), pr), ev.Repository)
	}
	return ghWithRepo(fmt.Sprintf("**%s** %s %s",
		ghLogin(ev.Sender), mdInertText(verb, glActorMax), pr),
		ev.Repository)
}

func renderGitHubIssues(ev *ghIssuesEvent) string {
	verb := ghIssueVerb(ev.Action)
	if verb == "" {
		return ""
	}
	return ghWithRepo(fmt.Sprintf("**%s** %s issue [#%d %s](%s)",
		ghLogin(ev.Sender), mdInertText(verb, glActorMax), ev.Issue.Number,
		mdLinkText(ev.Issue.Title, 200), ev.Issue.HTMLURL),
		ev.Repository)
}

func renderGitHubIssueComment(ev *ghIssueCommentEvent) string {
	verb := ghCommentVerb(ev.Action)
	if verb == "" {
		return ""
	}
	line := ghWithRepo(fmt.Sprintf("**%s** %s [#%d %s](%s)",
		ghLogin(ev.Sender), mdInertText(verb, glActorMax), ev.Issue.Number,
		mdLinkText(ev.Issue.Title, 200), ev.Comment.HTMLURL),
		ev.Repository)
	if ev.Action == "deleted" {
		// 已删除的评论没有正文可引用。
		return line
	}
	if snippet := clipRunes(oneLine(ev.Comment.Body), 300); snippet != "" {
		line += "\n> " + snippet
	}
	return line
}

func renderGitHubRelease(ev *ghReleaseEvent) string {
	verb := ghReleaseVerb(ev.Action)
	if verb == "" {
		return ""
	}
	title := ev.Release.Name
	if strings.TrimSpace(title) == "" {
		title = ev.Release.TagName
	}
	return ghWithRepo(fmt.Sprintf("**%s** %s release [%s](%s)",
		ghLogin(ev.Sender), mdInertText(verb, glActorMax), mdLinkText(title, 200), ev.Release.HTMLURL),
		ev.Repository)
}

// ghPRVerb 把 GitHub pull_request 的 action 映射为渲染动词。所有 action 都会渲染
// （产品决定，与 GitLab glActionVerb 同一决定）——返回空仅表示 action 字段本身缺省，
// 这是唯一保留的过滤。已知值给出通顺的英文；未知/GitHub 未来新增的值原样透传。
//
// ⚠️ 未知值分支直接回传外部输入本身——调用方必须在拼进 markdown/card 前用
// mdInertText / escapeCardText 转义这个返回值，不能假设它总是字面量安全（与
// glActionVerb 同一契约，见 GitLab #610 review 的教训）。
func ghPRVerb(action string, merged bool) string {
	switch action {
	case "":
		return ""
	case "opened":
		return "opened"
	case "reopened":
		return "reopened"
	case "closed":
		if merged {
			return "merged"
		}
		return "closed"
	case "ready_for_review":
		return "marked ready for review"
	case "converted_to_draft":
		return "converted to draft"
	case "synchronize":
		return "pushed new commits to"
	case "edited":
		return "edited"
	case "labeled":
		return "labeled"
	case "unlabeled":
		return "unlabeled"
	case "assigned":
		return "assigned"
	case "unassigned":
		return "unassigned"
	case "review_requested":
		return "requested review on"
	case "review_request_removed":
		return "removed review request on"
	case "locked":
		return "locked"
	case "unlocked":
		return "unlocked"
	default:
		return action
	}
}

// ghIssueVerb 同 ghPRVerb，覆盖 issues 事件的 action。同一转义契约。
func ghIssueVerb(action string) string {
	switch action {
	case "":
		return ""
	case "opened":
		return "opened"
	case "closed":
		return "closed"
	case "reopened":
		return "reopened"
	case "edited":
		return "edited"
	case "labeled":
		return "labeled"
	case "unlabeled":
		return "unlabeled"
	case "assigned":
		return "assigned"
	case "unassigned":
		return "unassigned"
	case "pinned":
		return "pinned"
	case "unpinned":
		return "unpinned"
	case "locked":
		return "locked"
	case "unlocked":
		return "unlocked"
	default:
		return action
	}
}

// ghCommentVerb 同 ghPRVerb，覆盖 issue_comment 事件的 action。已知值直接拼出完整
// 动词短语（含 "on"/"a comment on"），使调用方 "**actor** VERB [#N title]" 的拼接
// 模板保持一致。同一转义契约。
func ghCommentVerb(action string) string {
	switch action {
	case "":
		return ""
	case "created":
		return "commented on"
	case "edited":
		return "edited a comment on"
	case "deleted":
		return "deleted a comment on"
	default:
		return action
	}
}

// ghReleaseVerb 同 ghPRVerb，覆盖 release 事件的 action。同一转义契约。
func ghReleaseVerb(action string) string {
	switch action {
	case "":
		return ""
	case "published":
		return "published"
	case "unpublished":
		return "unpublished"
	case "created":
		return "created"
	case "edited":
		return "edited"
	case "deleted":
		return "deleted"
	case "prereleased":
		return "pre-released"
	case "released":
		return "released"
	default:
		return action
	}
}

// ghLogin 兜底空 sender（GitHub 偶发不带 sender，如某些 App 触发的事件），非空时
// 经 mdInertText 转义。login 字符集虽受 GitHub 官方限制，但本端点不校验请求真的来自
// GitHub（只查共享 URL token，无强制 HMAC——见文件头注释），持有 token 的调用方能把
// login 设成任意字符串；不转义会重现 GitLab glActor 的 username 注入（GitLab #610
// review，yujiawei 发现）。card 路径的 ghActorCard 一直就转义，这里补齐文本路径。
func ghLogin(u ghUser) string {
	if u.Login == "" {
		return "someone"
	}
	return mdInertText(u.Login, glActorMax)
}

// ghWithRepo 给消息行追加 " · [repo](url)" 尾注；repo 信息缺失时原样返回。
// FullName 经 mdInertText 转义——同 ghLogin 的理由，不能假设仓库全名字面量安全
// （与 GitLab glWithRepo 对 PathWithNamespace 的处理同口径）。
func ghWithRepo(line string, r ghRepo) string {
	if r.FullName == "" {
		return line
	}
	name := mdInertText(r.FullName, 200)
	if r.HTMLURL == "" {
		return line + " · " + name
	}
	return fmt.Sprintf("%s · [%s](%s)", line, name, r.HTMLURL)
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

// ============================================================
// Card rendering (card-message webhook-cardmsg-adapter)
// ============================================================
//
// The card builders below mirror the text renderers' event/action decisions but emit
// an octo/v1 card object (see adapter_card.go). They operate on the SAME *ev the text
// renderer used (parseGitHubPush unmarshals once), and return nil for any
// event/action outside the rendered subset — nil degrades to text via vcsPushReq.

// ghActorCard is ghLogin for the card path: escaped for a TextBlock leaf (a GitHub
// login has a restricted charset, but escaping is harmless and keeps parity with the
// free-text GitLab display name).
func ghActorCard(u ghUser) string {
	if u.Login == "" {
		return "someone"
	}
	return escapeCardText(u.Login, cardActorMax)
}

// ghRepoURLCard prefers the push compare URL, else the repo HTML URL, each through
// the http(s) allowlist ("" when neither is a valid absolute http(s) URL → button
// omitted).
func ghRepoURLCard(compare string, r ghRepo) string {
	if u := httpURLForCard(compare); u != "" {
		return u
	}
	return httpURLForCard(r.HTMLURL)
}

func buildGitHubPushCard(ev *ghPushEvent, lang string) map[string]interface{} {
	who := ghActorCard(ev.Sender)
	ref := cardCodeSpan(ghShortRef(ev.Ref), cardRefMax)
	d := vcsCardData{
		source:   cardSourceGitHub,
		variant:  "vcs.github.push",
		subtitle: escapeCardText(ev.Repository.FullName, cardTitleMax),
		url:      ghRepoURLCard(ev.Compare, ev.Repository),
	}
	switch {
	case strings.HasPrefix(ev.Ref, "refs/tags/"):
		if ev.Deleted {
			d.headline = fmt.Sprintf("%s deleted tag %s", who, ref)
		} else {
			d.headline = fmt.Sprintf("%s pushed tag %s", who, ref)
		}
	case ev.Deleted:
		d.headline = fmt.Sprintf("%s deleted branch %s", who, ref)
	case ev.Created && len(ev.Commits) == 0:
		d.headline = fmt.Sprintf("%s created branch %s", who, ref)
	case len(ev.Commits) == 0:
		// 退化 ref 更新（无提交、非建/删）：文本路径 skip，卡片同样不产出。
		return nil
	default:
		verb := "pushed"
		if ev.Forced {
			verb = "force-pushed"
		}
		d.headline = fmt.Sprintf("%s %s %d commit(s) to %s", who, verb, len(ev.Commits), ref)
		for i, cm := range ev.Commits {
			if i == maxRenderedCommits {
				d.lines = append(d.lines, fmt.Sprintf("…and %d more", len(ev.Commits)-maxRenderedCommits))
				break
			}
			d.lines = append(d.lines, joinShaMsg(
				cardCodeSpan(ghShortSHA(cm.ID), cardShaMax),
				escapeCardText(firstLine(cm.Message), cardCommitMsgMax)))
		}
	}
	return d.card(lang)
}

func buildGitHubPullRequestCard(ev *ghPullRequestEvent, lang string) map[string]interface{} {
	verb := ghPRVerb(ev.Action, ev.PullRequest.Merged)
	if verb == "" {
		return nil
	}
	// 卡片专属的结构化 FactSet（源分支 / 目标分支 / 标签）——文本路径不含这些字段，
	// 故 flag-off 字节不变，与 GitLab MR 卡片同一约定（见 buildGitLabMergeRequestCard）。
	labels := vcsCardLabelsFor(lang)
	var facts []vcsFact
	if src := cardCodeSpan(ev.PullRequest.Head.Ref, cardRefMax); src != "" {
		facts = append(facts, vcsFact{title: labels.source, value: src})
	}
	if tgt := cardCodeSpan(ev.PullRequest.Base.Ref, cardRefMax); tgt != "" {
		facts = append(facts, vcsFact{title: labels.target, value: tgt})
	}
	if f := ghLabelsFact(labels.labels, ev.PullRequest.Labels); f != nil {
		facts = append(facts, *f)
	}
	var headline string
	switch ev.Action {
	case "ready_for_review":
		headline = fmt.Sprintf("%s marked a pull request ready for review", ghActorCard(ev.Sender))
	case "converted_to_draft":
		headline = fmt.Sprintf("%s converted a pull request to draft", ghActorCard(ev.Sender))
	default:
		headline = fmt.Sprintf("%s %s a pull request", ghActorCard(ev.Sender), escapeCardText(verb, cardActorMax))
	}
	return vcsCardData{
		source:   cardSourceGitHub,
		variant:  "vcs.github.pull_request",
		headline: headline,
		subtitle: escapeCardText(ev.Repository.FullName, cardTitleMax),
		lines:    []string{numberedTitle("#", ev.PullRequest.Number, ev.PullRequest.Title)},
		facts:    facts,
		url:      httpURLForCard(ev.PullRequest.HTMLURL),
	}.card(lang)
}

func buildGitHubIssuesCard(ev *ghIssuesEvent, lang string) map[string]interface{} {
	verb := ghIssueVerb(ev.Action)
	if verb == "" {
		return nil
	}
	var facts []vcsFact
	if f := ghLabelsFact(vcsCardLabelsFor(lang).labels, ev.Issue.Labels); f != nil {
		facts = append(facts, *f)
	}
	return vcsCardData{
		source:   cardSourceGitHub,
		variant:  "vcs.github.issues",
		headline: fmt.Sprintf("%s %s an issue", ghActorCard(ev.Sender), escapeCardText(verb, cardActorMax)),
		subtitle: escapeCardText(ev.Repository.FullName, cardTitleMax),
		lines:    []string{numberedTitle("#", ev.Issue.Number, ev.Issue.Title)},
		facts:    facts,
		url:      httpURLForCard(ev.Issue.HTMLURL),
	}.card(lang)
}

// ghLabelsFact builds the shared "Labels (N)" FactSet row for the PR/Issue cards
// (nil when there is nothing to show), mirroring GitLab's glLabelsFact — same
// escaping/capping contract via the shared cappedFactValue helper.
func ghLabelsFact(title string, labels []ghLabel) *vcsFact {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	value, n := cappedFactValue(names, maxRenderedLabels)
	if n == 0 {
		return nil
	}
	return &vcsFact{title: fmt.Sprintf("%s (%d)", title, n), value: value}
}

func buildGitHubIssueCommentCard(ev *ghIssueCommentEvent, lang string) map[string]interface{} {
	verb := ghCommentVerb(ev.Action)
	if verb == "" {
		return nil
	}
	d := vcsCardData{
		source:   cardSourceGitHub,
		variant:  "vcs.github.issue_comment",
		headline: fmt.Sprintf("%s %s an issue", ghActorCard(ev.Sender), escapeCardText(verb, cardActorMax)),
		subtitle: escapeCardText(ev.Repository.FullName, cardTitleMax),
		lines:    []string{numberedTitle("#", ev.Issue.Number, ev.Issue.Title)},
		url:      httpURLForCard(ev.Comment.HTMLURL),
	}
	if ev.Action != "deleted" {
		d.quote = escapeCardText(ev.Comment.Body, cardQuoteMax)
	}
	return d.card(lang)
}

func buildGitHubReleaseCard(ev *ghReleaseEvent, lang string) map[string]interface{} {
	verb := ghReleaseVerb(ev.Action)
	if verb == "" {
		return nil
	}
	title := ev.Release.Name
	if strings.TrimSpace(title) == "" {
		title = ev.Release.TagName
	}
	return vcsCardData{
		source:   cardSourceGitHub,
		variant:  "vcs.github.release",
		headline: fmt.Sprintf("%s %s a release", ghActorCard(ev.Sender), escapeCardText(verb, cardActorMax)),
		subtitle: escapeCardText(ev.Repository.FullName, cardTitleMax),
		lines:    []string{escapeCardText(title, cardTitleMax)},
		url:      httpURLForCard(ev.Release.HTMLURL),
	}.card(lang)
}
