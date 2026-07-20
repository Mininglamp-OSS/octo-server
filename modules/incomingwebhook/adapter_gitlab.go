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
// （走 native 纯文本路径）。MR/Issue 的所有 action、pipeline 的所有 status 均会渲染
// （产品决定：不按「是否刷屏」过滤——旧版本只渲染终态/open-close-reopen-merge 子集，
// 现已放开）；仍在渲染子集之外的只有事件【类型】本身（Job Hook / Wiki Page Hook 等，
// 见 parseGitLabPush 的 default 分支）与畸形 payload（缺 action/status 字段）。所有
// gl* 结构体只声明渲染需要的字段（白名单解析），其余 payload 字段一律忽略。

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
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

// glLabel is a GitLab label object (the top-level `labels[]` array GitLab attaches
// to Merge Request Hook / Issue Hook payloads). Only Title is rendered.
type glLabel struct {
	Title string `json:"title"`
}

type glMergeRequestEvent struct {
	User             glUser `json:"user"`
	ObjectAttributes struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Action string `json:"action"`
		// SourceBranch / TargetBranch feed the card-only Source/Target FactSet rows
		// (text path unchanged, same "card-only" convention as pipeline's
		// Duration/Jobs — see glPipelineEvent).
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
	} `json:"object_attributes"`
	// Labels feeds the card-only Labels FactSet row;缺省即不展示该行。
	Labels  []glLabel `json:"labels"`
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
	// Labels feeds the card-only Labels FactSet row;缺省即不展示该行。
	Labels  []glLabel `json:"labels"`
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
		// Duration 是流水线耗时（秒，GitLab 在终态事件里给出）。仅卡片路径渲染
		// （文本路径保持历史输出不变）。可能缺省(0)/为 null → 不展示。
		Duration float64 `json:"duration"`
	} `json:"object_attributes"`
	User    glUser    `json:"user"`
	Project glProject `json:"project"`
	// Builds 是本次流水线的作业列表（GitLab 在 Pipeline Hook 里给出）。仅卡片路径用于
	// "Jobs (N)" 事实行；缺省即不展示该行。
	Builds []glBuild `json:"builds"`
}

// glBuild 是流水线里的单个作业（白名单解析：只取渲染需要的字段）。
type glBuild struct {
	Name   string `json:"name"`
	Status string `json:"status"`
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

	// card-message webhook-cardmsg-adapter：开关开时渲成 InteractiveCard(=17)，关闭
	//（或卡片自校验失败）时 vcsPushReq 降级回 markdown 文本路径（文本渲染器输出不变，
	// flag-off 字节与历史一致）。body 每事件【只反序列化一次】，文本与卡片共用同一 *ev。
	// 与 github 适配器同一套卡片骨架 / 转义器（parity）。
	wantCard := cardmsg.Enabled()
	lang := ""
	if wantCard {
		lang = i18n.OutboundLanguage(context.Background())
	}
	var content string
	var card map[string]interface{}
	switch event {
	case "Push Hook":
		var ev glPushEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitLabPush(&ev)
		if content != "" && wantCard {
			card = buildGitLabPushCard(&ev, lang)
		}
	case "Tag Push Hook":
		var ev glPushEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitLabTagPush(&ev)
		if content != "" && wantCard {
			card = buildGitLabTagPushCard(&ev, lang)
		}
	case "Merge Request Hook":
		var ev glMergeRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitLabMergeRequest(&ev)
		if content != "" && wantCard {
			card = buildGitLabMergeRequestCard(&ev, lang)
		}
	case "Issue Hook":
		var ev glIssueEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitLabIssue(&ev)
		if content != "" && wantCard {
			card = buildGitLabIssueCard(&ev, lang)
		}
	case "Note Hook":
		var ev glNoteEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitLabNote(&ev)
		if content != "" && wantCard {
			card = buildGitLabNoteCard(&ev, lang)
		}
	case "Pipeline Hook":
		var ev glPipelineEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, "", "json"
		}
		content = renderGitLabPipeline(&ev)
		if content != "" && wantCard {
			card = buildGitLabPipelineCard(&ev, lang)
		}
	default:
		// 渲染子集之外的事件类型（Job Hook / Wiki Page Hook / ...）：通常只是订阅范围
		// 大于我们渲染的子集，调用方无需修复 → 200 + skipped。
		return nil, "event", ""
	}
	if content == "" {
		// 事件类型支持、但动作不在渲染子集内（MR update / pipeline running / ...）：skip。
		return nil, "event", ""
	}
	return vcsPushReq(content, card), "", ""
}

// renderGitLabPush and its siblings render the text-path markdown from the shared
// *ev (parseGitLabPush unmarshals once). "" means the event/action is outside the
// rendered subset (caller treats "" as skip).
func renderGitLabPush(ev *glPushEvent) string {
	who := glActor(ev.UserUsername, ev.UserName)
	ref := glShortRef(ev.Ref)
	switch {
	case glIsZeroSHA(ev.After):
		return glWithRepo(fmt.Sprintf("**%s** deleted branch `%s`", who, ref), ev.Project)
	case glIsZeroSHA(ev.Before) && len(ev.Commits) == 0:
		return glWithRepo(fmt.Sprintf("**%s** created branch `%s`", who, ref), ev.Project)
	case len(ev.Commits) == 0:
		// 退化 ref 更新（无提交、非建/删）：渲染 "pushed 0 commit(s)" 只是噪音 → skip。
		return ""
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
	return b.String()
}

func renderGitLabTagPush(ev *glPushEvent) string {
	who := glActor(ev.UserUsername, ev.UserName)
	tag := glShortRef(ev.Ref)
	if glIsZeroSHA(ev.After) {
		return glWithRepo(fmt.Sprintf("**%s** deleted tag `%s`", who, tag), ev.Project)
	}
	return glWithRepo(fmt.Sprintf("**%s** pushed tag `%s`", who, tag), ev.Project)
}

func renderGitLabMergeRequest(ev *glMergeRequestEvent) string {
	verb := glActionVerb(ev.ObjectAttributes.Action)
	if verb == "" {
		// 缺 action 字段的畸形 payload：没有可渲染的动作 → skip（唯一保留的过滤）。
		return ""
	}
	// verb 可能是 glActionVerb 未知动作时原样透传的外部 action 值：拼进
	// `**actor** verb merge request` 纯文本前必须转义，否则一个恶意 action（如
	// `**pwn** [x](http://evil)`）能伪造粗体/可点击链接——与 actor/title 等外部字段
	// 同一套 mdInertText 处理（trust-boundary.md）。
	return glWithRepo(fmt.Sprintf("**%s** %s merge request [!%d %s](%s)",
		glActor(ev.User.Username, ev.User.Name), mdInertText(verb, glActorMax), ev.ObjectAttributes.IID,
		mdLinkText(ev.ObjectAttributes.Title, 200), ev.ObjectAttributes.URL),
		ev.Project)
}

func renderGitLabIssue(ev *glIssueEvent) string {
	verb := glActionVerb(ev.ObjectAttributes.Action)
	if verb == "" {
		return ""
	}
	return glWithRepo(fmt.Sprintf("**%s** %s issue [#%d %s](%s)",
		glActor(ev.User.Username, ev.User.Name), mdInertText(verb, glActorMax), ev.ObjectAttributes.IID,
		mdLinkText(ev.ObjectAttributes.Title, 200), ev.ObjectAttributes.URL),
		ev.Project)
}

func renderGitLabNote(ev *glNoteEvent) string {
	if ev.ObjectAttributes.System {
		// 系统备注（改标签/指派/状态等自动生成）：与 GitHub 只渲染人写评论一致，skip。
		return ""
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
	return line
}

func renderGitLabPipeline(ev *glPipelineEvent) string {
	if ev.ObjectAttributes.Status == "" {
		// 缺 status 字段的畸形 payload：没有可渲染的状态 → skip（唯一保留的过滤——
		// 所有非空状态，包括 pending/running/created/manual/skipped，均渲染）。
		return ""
	}
	// Pipeline 是唯一自拼 URL 的事件（MR/Issue/Note 直接用 object_attributes.url 绝对
	// 地址）。project.web_url 缺失时（白名单解析不保证字段必到）退化为不带链接的纯文本，
	// 避免渲染出 [#99](/-/pipelines/99) 这种不可点击的相对路径（#423 review，lml2468）。
	//
	// status 曾经只可能是 switch 已放行的三个终态字面量（安全），过滤放开后它是外部原样
	// 输入（任何持有 URL token 的调用方都能自定义），拼进纯文本前必须转义——与
	// glActionVerb 的 verb 同一类「白名单收窄=隐式转义」陷阱，同一套 mdInertText 处理
	// （#610 review，lml2468 P1：这次过滤放开只修了 verb，漏了 status）。
	status := mdInertText(ev.ObjectAttributes.Status, glActorMax)
	var line string
	if ev.Project.WebURL != "" {
		line = fmt.Sprintf("Pipeline [#%d](%s/-/pipelines/%d) %s on `%s`",
			ev.ObjectAttributes.ID, ev.Project.WebURL, ev.ObjectAttributes.ID,
			status, glShortRef(ev.ObjectAttributes.Ref))
	} else {
		line = fmt.Sprintf("Pipeline #%d %s on `%s`",
			ev.ObjectAttributes.ID, status, glShortRef(ev.ObjectAttributes.Ref))
	}
	return glWithRepo(line, ev.Project)
}

// glActor 优先用 username，回退 display name，再兜底 "someone"。两者都经
// mdInertText 转义（`*`/`[`/`]`/`<` 等），否则一个名为 `**evil** [x](http://attacker)`
// 的 username/display name 能往群消息里注入粗体+可点击链接——与 adapter_multica.go
// 对 actor/identifier 的处理同口径（#423 review，Jerry-Xin/mochashanyao）。
//
// username 不能只按「GitLab 用户名字符集受限」豁免转义：本端点只校验共享密钥
// token，并不验证请求真的来自 GitLab，持有 token 的调用方能把 username 设成任意
// 字符串——与 action/status 曾经「白名单收窄=隐式安全」是同一类陷阱（#610 review，
// yujiawei 二审发现，预存在于本文件、随本次改动一并修复）。card 路径的 glActorCard
// 一直就是两个分支都转义，这里是把文本路径补齐到同一口径。
func glActor(username, name string) string {
	if username != "" {
		return mdInertText(username, glActorMax)
	}
	if name != "" {
		return mdInertText(name, glActorMax)
	}
	return "someone"
}

// glActionVerb 把 GitLab 的 MR/Issue object_attributes.action 映射为渲染动词。所有
// action 都会渲染（产品决定：不再按「是否刷屏」过滤，update/approved/unapproved 等
// 曾经跳过的动作现在也推送）——返回空仅表示 action 字段本身缺省（畸形 payload，没
// 有可渲染的动作），调用方据此走 skip，这是唯一保留的过滤。已知值给出通顺的英文
// 过去式；未知/GitLab 未来新增的值原样透传，避免每次 GitLab 加新 action 都要改代码。
//
// ⚠️ 未知值分支直接回传外部输入本身（action 字段无枚举校验，任何持有 URL token 的
// 调用方都能自定义）——调用方必须在拼进 markdown/card 前用 mdInertText / escapeCardText
// 转义这个返回值，不能假设它总是字面量安全。
func glActionVerb(action string) string {
	switch action {
	case "":
		return ""
	case "open":
		return "opened"
	case "close":
		return "closed"
	case "reopen":
		return "reopened"
	case "merge":
		return "merged"
	case "update":
		return "updated"
	case "approved":
		return "approved"
	case "unapproved":
		return "unapproved"
	default:
		return action
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

// ============================================================
// Card rendering (card-message webhook-cardmsg-adapter)
// ============================================================
//
// The card builders below mirror the text renderers' event/action decisions but emit
// an octo/v1 card object using the SAME anatomy + escaper as the github adapter
// (adapter_card.go) — parity. They operate on the SAME *ev the text renderer used
// (parseGitLabPush unmarshals once), returning nil only when the payload has nothing
// to render (missing action/status — see glActionVerb / buildGitLabPipelineCard),
// never because of which action/status it is (→ degrade to text via vcsPushReq).

// glActorCard is glActor for the card path: username (restricted charset) or the
// free-text display name, both escaped for a TextBlock leaf.
func glActorCard(username, name string) string {
	if username != "" {
		return escapeCardText(username, cardActorMax)
	}
	if name != "" {
		return escapeCardText(name, cardActorMax)
	}
	return "someone"
}

func buildGitLabPushCard(ev *glPushEvent, lang string) map[string]interface{} {
	who := glActorCard(ev.UserUsername, ev.UserName)
	ref := cardCodeSpan(glShortRef(ev.Ref), cardRefMax)
	d := vcsCardData{
		source:   cardSourceGitLab,
		variant:  "vcs.gitlab.push",
		subtitle: escapeCardText(ev.Project.PathWithNamespace, cardTitleMax),
		url:      httpURLForCard(ev.Project.WebURL),
	}
	switch {
	case glIsZeroSHA(ev.After):
		d.headline = fmt.Sprintf("%s deleted branch %s", who, ref)
	case glIsZeroSHA(ev.Before) && len(ev.Commits) == 0:
		d.headline = fmt.Sprintf("%s created branch %s", who, ref)
	case len(ev.Commits) == 0:
		return nil
	default:
		n := max(ev.TotalCommits, len(ev.Commits))
		d.headline = fmt.Sprintf("%s pushed %d commit(s) to %s", who, n, ref)
		for i, cm := range ev.Commits {
			if i == maxRenderedGitLabCommits {
				d.lines = append(d.lines, fmt.Sprintf("…and %d more", n-maxRenderedGitLabCommits))
				break
			}
			d.lines = append(d.lines, joinShaMsg(
				cardCodeSpan(glShortSHA(cm.ID), cardShaMax),
				escapeCardText(firstLine(cm.Message), cardCommitMsgMax)))
		}
	}
	return d.card(lang)
}

func buildGitLabTagPushCard(ev *glPushEvent, lang string) map[string]interface{} {
	who := glActorCard(ev.UserUsername, ev.UserName)
	tag := cardCodeSpan(glShortRef(ev.Ref), cardRefMax)
	headline := fmt.Sprintf("%s pushed tag %s", who, tag)
	if glIsZeroSHA(ev.After) {
		headline = fmt.Sprintf("%s deleted tag %s", who, tag)
	}
	return vcsCardData{
		source:   cardSourceGitLab,
		variant:  "vcs.gitlab.tag_push",
		headline: headline,
		subtitle: escapeCardText(ev.Project.PathWithNamespace, cardTitleMax),
		url:      httpURLForCard(ev.Project.WebURL),
	}.card(lang)
}

func buildGitLabMergeRequestCard(ev *glMergeRequestEvent, lang string) map[string]interface{} {
	verb := glActionVerb(ev.ObjectAttributes.Action)
	if verb == "" {
		return nil
	}
	// 卡片专属的结构化 FactSet（源分支 / 目标分支 / 标签）——文本路径不含这些字段，
	// 故 flag-off 字节不变，与 pipeline 卡片同一约定（见 buildGitLabPipelineCard）。
	labels := vcsCardLabelsFor(lang)
	var facts []vcsFact
	if src := cardCodeSpan(ev.ObjectAttributes.SourceBranch, cardRefMax); src != "" {
		facts = append(facts, vcsFact{title: labels.source, value: src})
	}
	if tgt := cardCodeSpan(ev.ObjectAttributes.TargetBranch, cardRefMax); tgt != "" {
		facts = append(facts, vcsFact{title: labels.target, value: tgt})
	}
	if f := glLabelsFact(labels.labels, ev.Labels); f != nil {
		facts = append(facts, *f)
	}
	return vcsCardData{
		source:   cardSourceGitLab,
		variant:  "vcs.gitlab.merge_request",
		headline: fmt.Sprintf("%s %s a merge request", glActorCard(ev.User.Username, ev.User.Name), escapeCardText(verb, cardActorMax)),
		subtitle: escapeCardText(ev.Project.PathWithNamespace, cardTitleMax),
		lines:    []string{numberedTitle("!", ev.ObjectAttributes.IID, ev.ObjectAttributes.Title)},
		facts:    facts,
		url:      httpURLForCard(ev.ObjectAttributes.URL),
	}.card(lang)
}

func buildGitLabIssueCard(ev *glIssueEvent, lang string) map[string]interface{} {
	verb := glActionVerb(ev.ObjectAttributes.Action)
	if verb == "" {
		return nil
	}
	var facts []vcsFact
	if f := glLabelsFact(vcsCardLabelsFor(lang).labels, ev.Labels); f != nil {
		facts = append(facts, *f)
	}
	return vcsCardData{
		source:   cardSourceGitLab,
		variant:  "vcs.gitlab.issue",
		headline: fmt.Sprintf("%s %s an issue", glActorCard(ev.User.Username, ev.User.Name), escapeCardText(verb, cardActorMax)),
		subtitle: escapeCardText(ev.Project.PathWithNamespace, cardTitleMax),
		lines:    []string{numberedTitle("#", ev.ObjectAttributes.IID, ev.ObjectAttributes.Title)},
		facts:    facts,
		url:      httpURLForCard(ev.ObjectAttributes.URL),
	}.card(lang)
}

// glLabelsFact builds the shared "Labels (N)" FactSet row for the MR/Issue cards
// (nil when there is nothing to show — no labels, or every label title is blank —
// so the caller omits the row entirely). Label titles are project-defined free text,
// escaped/capped by the shared cappedFactValue (same convention as the pipeline
// card's Jobs fact); the count in the title reflects the real (non-blank) total, not
// the truncated list.
func glLabelsFact(title string, labels []glLabel) *vcsFact {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Title
	}
	value, n := cappedFactValue(names, maxRenderedLabels)
	if n == 0 {
		return nil
	}
	return &vcsFact{title: fmt.Sprintf("%s (%d)", title, n), value: value}
}

func buildGitLabNoteCard(ev *glNoteEvent, lang string) map[string]interface{} {
	if ev.ObjectAttributes.System {
		return nil
	}
	var target string
	switch ev.ObjectAttributes.NoteableType {
	case "MergeRequest":
		target = numberedTitle("!", ev.MergeRequest.IID, ev.MergeRequest.Title)
	case "Issue":
		target = numberedTitle("#", ev.Issue.IID, ev.Issue.Title)
	case "Commit":
		target = "commit " + cardCodeSpan(glShortSHA(ev.Commit.ID), cardShaMax)
	default:
		target = "a comment"
	}
	return vcsCardData{
		source:   cardSourceGitLab,
		variant:  "vcs.gitlab.note",
		headline: fmt.Sprintf("%s commented", glActorCard(ev.User.Username, ev.User.Name)),
		subtitle: escapeCardText(ev.Project.PathWithNamespace, cardTitleMax),
		lines:    []string{target},
		quote:    escapeCardText(ev.ObjectAttributes.Note, cardQuoteMax),
		url:      httpURLForCard(ev.ObjectAttributes.URL),
	}.card(lang)
}

func buildGitLabPipelineCard(ev *glPipelineEvent, lang string) map[string]interface{} {
	if ev.ObjectAttributes.Status == "" {
		return nil
	}
	// 卡片专属的结构化 FactSet（分支 / 状态 / 耗时 / 作业）——文本路径不含这些字段，
	// 故 flag-off 字节不变。标签本地化（内容标签，非 errcode），值在叶子处转义。
	labels := pipelineLabelsFor(lang)
	facts := []vcsFact{
		{title: labels.branch, value: escapeCardText(glShortRef(ev.ObjectAttributes.Ref), cardRefMax)},
		{title: labels.status, value: escapeCardText(ev.ObjectAttributes.Status, cardActorMax)},
	}
	if dur := formatPipelineDuration(int(ev.ObjectAttributes.Duration)); dur != "" {
		facts = append(facts, vcsFact{title: labels.duration, value: dur})
	}
	if len(ev.Builds) > 0 {
		names := make([]string, len(ev.Builds))
		for i, b := range ev.Builds {
			names[i] = b.Name
		}
		if value, n := cappedFactValue(names, maxRenderedJobs); n > 0 {
			facts = append(facts, vcsFact{title: fmt.Sprintf("%s (%d)", labels.jobs, n), value: value})
		}
	}
	url := ""
	if p := httpURLForCard(ev.Project.WebURL); p != "" {
		url = fmt.Sprintf("%s/-/pipelines/%d", strings.TrimRight(p, "/"), ev.ObjectAttributes.ID)
	}
	return vcsCardData{
		source:   cardSourceGitLab,
		variant:  "vcs.gitlab.pipeline",
		headline: fmt.Sprintf("Pipeline #%d", ev.ObjectAttributes.ID),
		status:   pipelineStatusColor(ev.ObjectAttributes.Status),
		subtitle: escapeCardText(ev.Project.PathWithNamespace, cardTitleMax),
		facts:    facts,
		url:      url,
	}.card(lang)
}
