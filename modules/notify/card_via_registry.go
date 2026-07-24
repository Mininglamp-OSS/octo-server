package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	"go.uber.org/zap"
)

// errCardTmplUnavailable 是 preflight / build 通路上 "Registry 未注入 / Template
// 未注册" 的 typed 内部错误。caller (deliverDocsCardNotification) 对它走
// **500 不降级**——这是 composition bug,不应把错请求伪装成成功文本 (§10 / A14)。
// 与 cardtmpl.ErrFieldsInvalid 对齐 typed:前者是 500,后者是 400。
var errCardTmplUnavailable = errors.New("notify: cardtmpl registry/template unavailable (composition bug)")

// templateFallbackText 是 F6 分工:access_requested + gate + Registry-ready 时,
// buildDocsFallbackText 会先调本函数,让 fallback 文本走 pilot Template 的 L0
// 定义。Registry 未注入 / Template 未注册 / mapping/unmarshal 失败 → 返 ok=false,
// caller 兜回 legacy 多行组装。
func templateFallbackText(card *DocsCardFields, lang string) (string, bool) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return "", false
	}
	tmpl, err := registry.Lookup(docsaccessrequest.TemplateID, "")
	if err != nil {
		return "", false
	}
	fields, err := mapDocsCardFieldsToJSON(card, lang)
	if err != nil {
		return "", false
	}
	text, err := tmpl.FallbackText(docsaccessrequest.StatePending, fields, lang)
	if err != nil || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// preflightDocsAccessRequestSchema 在 access_requested + gate 开的通路上,在
// memberCache / docsSender / cardmsg.Enabled 任意 gate 之前独立跑一次 pilot
// Template 的 InputSchema 校验。返回值分类:
//   - schema 不合法 → cardtmpl.ErrFieldsInvalid (400 零投递,C1)
//   - Registry / Template 未注入 → errCardTmplUnavailable (500 不降级)
//   - nil → 通过
//
// R2-2 修正:前版当 registry nil 时返 nil 让 caller 继续走 v2 分支,后者最终降级
// 文本 —— 那样"错请求"仍然会成功送达一条文本,与 §10 / A14 冲突。现在 preflight
// 阶段就报 500,让 wiring bug 立刻可见。
func preflightDocsAccessRequestSchema(card *DocsCardFields) error {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}
	tmpl, err := registry.Lookup(docsaccessrequest.TemplateID, "")
	if err != nil {
		return fmt.Errorf("%w: lookup %s: %v", errCardTmplUnavailable, docsaccessrequest.TemplateID, err)
	}
	fields, err := mapDocsCardFieldsToJSON(card, "zh-CN") // preflight 只关心 schema shape,lang 无差异
	if err != nil {
		return fmt.Errorf("%w: map: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	var parsed any
	if err := json.Unmarshal(fields, &parsed); err != nil {
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	if err := tmpl.Meta().InputSchema.Validate(parsed); err != nil {
		// R2-8: preflight schema 失败也打 fields_invalid metric (Render 未被触发,
		// 否则 metric 永远漏这条最"合法"的 400 路径)。
		cardtmpl.RecordFieldsInvalid(docsaccessrequest.TemplateID, tmpl.Meta().Version)
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	// R3-1: schema 的 avatarUrl pattern 只做粗粒度前缀防护,正则无法可靠判定 host
	// 存在 —— "https://?x" / "https://#y" 的 host 为空却能过 pattern,随后在 Build
	// 内 requireHTTPS 才失败、被误分类成 render_error 降级文本 (绕过 C1 400)。
	// 这里用与 Build 同一个 URL parser (cardtmpl.AbsoluteHTTPSURL) 在 ingress 前置
	// 断言绝对 https,把坏字段确定性收敛成 C1 400,零缝隙、零降级。空头像合法 (省略头像列)。
	//
	// 维护约束:本校验按字段硬编码 (当前 pilot 唯一的用户可控 URL 字段是 avatar;
	// Source.IconURL 由服务端置空)。**L1 schema 若新增任何 URL 字段,必须在此同步
	// 补 AbsoluteHTTPSURL 校验**,否则该字段又会退回"schema 过 → Build 失败 →
	// render_error 降级"的同类缝隙。未来可由 Template 声明"须绝对 https 的字段集"
	// 让基座统一前置校验,消除这条硬编码耦合。
	if avatar := strings.TrimSpace(card.ActorAvatarURL); avatar != "" {
		if err := cardtmpl.AbsoluteHTTPSURL(avatar); err != nil {
			cardtmpl.RecordFieldsInvalid(docsaccessrequest.TemplateID, tmpl.Meta().Version)
			return fmt.Errorf("%w: actor_avatar_url: %v", cardtmpl.ErrFieldsInvalid, err)
		}
	}
	return nil
}

// buildDocsAccessRequestCardViaRegistry 走 L0 Registry.Render 生成
// docs.access-request 卡片。相较 buildDocsAccessRequestCard 差异:
//   - metadata.octo 新增 {protocol, template:{id,version}} 由基座强制注入;
//   - schema 校验失败返回 typed cardtmpl.ErrFieldsInvalid (由 caller 翻 400,C1);
//   - Registry/Template 未注入返 errCardTmplUnavailable (R2-2:caller 翻 500 不降级);
//   - 其他 render 级错 non-typed,caller 走 render_error 降级为纯文本 (F6/§10)。
//
// R2-2 更正:DefaultRegistry nil 走 typed errCardTmplUnavailable —— 与 preflight
// 同分类,让 wiring bug 在 caller 那里映射到 500 不降级。
func (n *Notify) buildDocsAccessRequestCardViaRegistry(
	ctx context.Context, spaceID string, card *DocsCardFields, lang string,
) (json.RawMessage, string, error) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		n.Error("cardtmpl DefaultRegistry unwired — composition bug",
			zap.String("space_id", spaceID), zap.String("doc_id", card.DocID))
		return nil, "", fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}

	fields, err := mapDocsCardFieldsToJSON(card, lang)
	if err != nil {
		return nil, "", fmt.Errorf("notify: map DocsCardFields to schema JSON: %w", err)
	}

	env := cardtmpl.BuildEnv{
		WebLoginURL: n.ctx.GetConfig().External.WebLoginURL,
		Lang:        lang,
		SpaceID:     spaceID,
	}
	cardDoc, _, renderProfile, err := registry.RenderCardWithProfiles(ctx,
		docsaccessrequest.TemplateID, "",
		docsaccessrequest.StatePending, fields, env)
	if err != nil {
		return nil, "", err
	}
	return cardDoc, renderProfile, nil
}

// docsActorNameMaxRunes / docsUpdatedAtMaxRunes mirror the docs.commented /
// docs.shared data.schema.json maxLength for these display fields.
// mapDocsCardFieldsToDisplayJSON truncates to them (title/excerpt reuse the
// exported cardtmpl caps) so an over-length display string renders truncated
// rather than flipping to a C1 400 (P1, PR #649 review). The schema maxLength is
// the backstop — TestSchemaCapsMatchRenderCaps (docs_commented/docs_shared)
// asserts no drift, and TestMapDocsDisplayFields_TruncatesDisplayFields locks
// that truncation leaves the field within schema.
const (
	docsActorNameMaxRunes = 120
	docsUpdatedAtMaxRunes = 80
)

// mapDocsCardFieldsToDisplayJSON 把扁平 DocsCardFields 映射成 docs.commented /
// docs.shared 契约期望的 JSON 形状(与 pilot mapDocsCardFieldsToJSON 同思路,
// 只是纯展示卡的字段集更小 —— 无 requestId/permission/头像/申请理由)。字节
// 等价基线依赖此映射与 legacy buildDocsCard 提取自同一 DocsCardFields。
//
// P1 (PR #649 review):展示字段服务端截断到 schema maxLength / 渲染预算。legacy
// buildDocsCard 从不按长度拒 —— 超长 excerpt 在 render 时 truncateRunes 截断后照常
// 投递。走 C1 preflight 后,超长字段本会翻成 400 零投递(连 text 兜底都不触发)。
// 这里截断保住"通知永不丢"不变量,同时对 docId(deep-link key,绝不能静默截断)
// 保留 C1 硬 400 —— 与 summary 侧 taskNo 同处置(见 mapSummaryCardFieldsToJSON)。
func mapDocsCardFieldsToDisplayJSON(card *DocsCardFields) (json.RawMessage, error) {
	if card == nil {
		return nil, errors.New("mapDocsCardFieldsToDisplayJSON: nil card")
	}
	m := map[string]any{
		"docId":     strings.TrimSpace(card.DocID), // deep-link key:不截断(超长 → C1 400)
		"title":     cardtmpl.TruncateRunes(strings.TrimSpace(card.Title), cardtmpl.MaxTitleRunes),
		"actorName": cardtmpl.TruncateRunes(strings.TrimSpace(card.ActorName), docsActorNameMaxRunes),
		"excerpt":   cardtmpl.TruncateRunes(strings.TrimSpace(card.Excerpt), cardtmpl.MaxExcerptRunes),
		"updatedAt": cardtmpl.TruncateRunes(strings.TrimSpace(card.UpdatedAt), docsUpdatedAtMaxRunes),
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped display fields: %w", err)
	}
	return json.RawMessage(raw), nil
}

// buildDocsDisplayCardViaRegistry 通过 Registry.Render 生成 docs.commented /
// docs.shared 展示卡(v1)。错误分类与 buildDocsAccessRequestCardViaRegistry 对齐:
//   - Registry 未注入 → errCardTmplUnavailable(caller 翻 500,不降级);
//   - schema 校验失败 → cardtmpl.ErrFieldsInvalid(caller 翻 400 零投递,C1);
//   - 其他 render 级错 → non-typed,caller 走 render_error 降级文本。
func (n *Notify) buildDocsDisplayCardViaRegistry(
	ctx context.Context, spaceID string, card *DocsCardFields, lang string,
	templateID cardtmpl.ID, state cardtmpl.State,
) (json.RawMessage, string, error) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return nil, "", fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}
	fields, err := mapDocsCardFieldsToDisplayJSON(card)
	if err != nil {
		return nil, "", fmt.Errorf("notify: map DocsCardFields to display schema JSON: %w", err)
	}
	env := cardtmpl.BuildEnv{
		WebLoginURL: n.ctx.GetConfig().External.WebLoginURL,
		Lang:        lang,
		SpaceID:     spaceID,
	}
	cardDoc, _, renderProfile, err := registry.RenderCardWithProfiles(ctx, templateID, "", state, fields, env)
	if err != nil {
		return nil, "", err
	}
	return cardDoc, renderProfile, nil
}

// preflightDocsDisplaySchema 与 preflightDocsAccessRequestSchema 同思路:在
// memberCache/docsSender/gate 之前独立跑一次 InputSchema 校验(C1 不被绕过)。
// docs.commented / docs.shared 无用户可控 URL 字段,不需要 avatar https 前置校验。
func preflightDocsDisplaySchema(card *DocsCardFields, templateID cardtmpl.ID) error {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}
	tmpl, err := registry.Lookup(templateID, "")
	if err != nil {
		return fmt.Errorf("%w: lookup %s: %v", errCardTmplUnavailable, templateID, err)
	}
	fields, err := mapDocsCardFieldsToDisplayJSON(card)
	if err != nil {
		return fmt.Errorf("%w: map: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	var parsed any
	if err := json.Unmarshal(fields, &parsed); err != nil {
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	if err := tmpl.Meta().InputSchema.Validate(parsed); err != nil {
		cardtmpl.RecordFieldsInvalid(templateID, tmpl.Meta().Version)
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	return nil
}

// mapDocsCardFieldsToJSON 把扁平 DocsCardFields 映射成 pilot data.schema.json 期望的
// 嵌套 JSON 形状。服务端字典字段(permission/document.sourceName/requestedAtDisplay/
// messageTimeDisplay)按当前收件人语言从本地化词表补齐;不接受调用方传入。
//
// state 恒 "pending" (本 PR pilot 只注册 pending view;approved/rejected 由
// standard_action_finalizer 生成,不走 ingress)。
func mapDocsCardFieldsToJSON(card *DocsCardFields, lang string) (json.RawMessage, error) {
	if card == nil {
		return nil, errors.New("mapDocsCardFieldsToJSON: nil card")
	}
	labels := docsLabelsFor(lang)

	m := map[string]any{
		"requestId": strings.TrimSpace(card.RequestID),
		"state":     "pending",
		"document": map[string]any{
			"docId":      strings.TrimSpace(card.DocID),
			"title":      strings.TrimSpace(card.Title),
			"sourceName": labels.sourceLabel, // "文档" / "Docs" — 与 legacy source label 同源
		},
		"requester": map[string]any{
			"name":      strings.TrimSpace(card.ActorName),
			"avatarUrl": strings.TrimSpace(card.ActorAvatarURL),
		},
		// permission 服务端字典缺省留空; pilot Template pendingLabels 默认词表
		// ("查看者" / "viewer") 与 legacy requestBannerSuffix 现值一致,保证
		// 迁移前后 banner 字节等价。
		"requestReason":      strings.TrimSpace(card.Excerpt),
		"requestedAtDisplay": strings.TrimSpace(card.UpdatedAt),
		"messageTimeDisplay": strings.TrimSpace(card.UpdatedAt),
	}
	// document.url 由服务端拼接 (docsDeepLink 在 cardtmpl 内做),这里留空,
	// schema 允许空;pilot Template Build 内部不读它,读的是 requestId + WebLoginURL。
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped fields: %w", err)
	}
	return json.RawMessage(raw), nil
}

// summaryTimeRangeMaxRunes / summaryGeneratedAtMaxRunes mirror the summary card
// data.schema.json maxLength for these display fields. mapSummaryCardFieldsToJSON
// truncates to them (title/reason reuse the exported cardtmpl caps) so an
// over-length display string renders truncated rather than flipping to a C1 400
// (P2-1, PR #650 review). The schema maxLength is the backstop —
// TestMapSummaryFields_TruncatesDisplayFields locks these in sync (truncation must
// leave the field within schema, else preflight would still 400).
//
// summaryMembersMax / summaryMsgCountMax mirror the summary card data.schema.json
// `maximum` for the two integer counters. mapSummaryCardFieldsToJSON clamps
// caller-supplied counts to [0, max] so an over-max count renders capped rather
// than flipping to a C1 400 zero-delivery (Jerry-Xin PR #650 re-review): legacy
// buildSummaryCard rendered any positive int, so the schema `maximum` would
// otherwise turn a huge value into a 400 at ingress — the same delivery
// regression the display-field truncation already fixed, applied symmetrically to
// the counts. The schema `maximum` is the backstop —
// TestMapSummaryFields_ClampsOverMaxCounts locks these in sync (a local cap above
// schema would still 400 after clamp, failing the test).
const (
	summaryTimeRangeMaxRunes   = 200
	summaryGeneratedAtMaxRunes = 80
	summaryMembersMax          = 1_000_000
	summaryMsgCountMax         = 1_000_000_000
)

// clampCount clamps a caller-supplied count to the schema's [0, max] range.
// Legacy buildSummaryCard omitted non-positive members/msgCount via a `> 0` guard
// and rendered any positive int; the schema's minimum:0 / maximum would otherwise
// 400 a negative sentinel or an over-max value at C1 preflight. Clamping to
// [0, max] reproduces the legacy omit-and-deliver behavior for negatives (the
// Template's `> 0` guard drops the 0 fact) while preserving delivery for an
// over-max count (rendered capped at max) instead of a C1 400 zero-delivery.
func clampCount(v, limit int) int {
	if v < 0 {
		return 0
	}
	if v > limit {
		return limit
	}
	return v
}

// mapSummaryCardFieldsToJSON 把扁平 SummaryCardFields 映射成 summary.completed /
// summary.failed 契约期望的 JSON 形状(与 mapDocsCardFieldsToDisplayJSON 同思路,
// 只是字段集不同)。Kind == failed 时保留 reason;completed 时省略 reason(其
// schema 里根本没这个字段, additionalProperties:false 会拒绝)。字节等价基线依
// 赖此映射与 legacy buildSummaryCard 提取自同一 SummaryCardFields。
//
// P2-1 (PR #650 review):展示字段服务端截断到 schema maxLength / 渲染预算,计数
// 字段 clamp 到 schema [0, maximum]。legacy buildSummaryCard 从不校验长度/上限 ——
// 超长 title/reason 仍会投递(截断后的卡或文本 DM),任意正整数 members/msgCount
// 也照常渲染。走 C1 preflight 后,超长/超上限字段本会翻成 400 零投递。这里截断/
// clamp 保住"通知永不丢"不变量,同时对**真正的结构性违规**(缺失/空必填、类型错、
// 未知字段、以及超长的 taskNo —— deep-link key,绝不能静默改写)保留 C1 硬 400。
//
// taskNo 原样透传(不 TrimSpace):它是 deep-link 的结构性 key,legacy
// buildSummaryCard 也用原值拼 /s/{taskNo}(见 card.go),trim 会静默改写身份字段并
// 让 padded 输入与 legacy 字节漂移(Jerry-Xin PR #650 re-review)。空/纯空白的
// taskNo 在 deliverCardNotification 入口即被 errNotifyCardInvalid 挡回,不会到这里。
func mapSummaryCardFieldsToJSON(card *SummaryCardFields) (json.RawMessage, error) {
	if card == nil {
		return nil, errors.New("mapSummaryCardFieldsToJSON: nil card")
	}
	m := map[string]any{
		"taskNo":      card.TaskNo, // deep-link key:原样透传(与 legacy 一致);超长 → C1 400,不 trim/截断
		"title":       cardtmpl.TruncateRunes(strings.TrimSpace(card.Title), cardtmpl.MaxTitleRunes),
		"timeRange":   cardtmpl.TruncateRunes(strings.TrimSpace(card.TimeRange), summaryTimeRangeMaxRunes),
		"members":     clampCount(card.Members, summaryMembersMax),
		"msgCount":    clampCount(card.MsgCount, summaryMsgCountMax),
		"generatedAt": cardtmpl.TruncateRunes(strings.TrimSpace(card.GeneratedAt), summaryGeneratedAtMaxRunes),
	}
	if card.Kind == SummaryCardKindFailed {
		m["reason"] = cardtmpl.TruncateRunes(strings.TrimSpace(card.Reason), cardtmpl.MaxExcerptRunes)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped summary fields: %w", err)
	}
	return json.RawMessage(raw), nil
}

// buildSummaryCardViaRegistry 通过 Registry.Render 生成 summary.completed /
// summary.failed 展示卡(v1)。错误分类与 buildDocsDisplayCardViaRegistry 对齐:
//   - Registry 未注入 → errCardTmplUnavailable(caller 翻 500,不降级);
//   - schema 校验失败 → cardtmpl.ErrFieldsInvalid(caller 翻 400 零投递,C1);
//   - 其他 render 级错 → non-typed,caller 走 render_error 降级文本。
func (n *Notify) buildSummaryCardViaRegistry(
	ctx context.Context, spaceID string, card *SummaryCardFields, lang string,
	templateID cardtmpl.ID, state cardtmpl.State,
) (json.RawMessage, string, error) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return nil, "", fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}
	fields, err := mapSummaryCardFieldsToJSON(card)
	if err != nil {
		return nil, "", fmt.Errorf("notify: map SummaryCardFields to schema JSON: %w", err)
	}
	env := cardtmpl.BuildEnv{
		WebLoginURL: n.ctx.GetConfig().External.WebLoginURL,
		Lang:        lang,
		SpaceID:     spaceID,
	}
	cardDoc, _, renderProfile, err := registry.RenderCardWithProfiles(ctx, templateID, "", state, fields, env)
	if err != nil {
		return nil, "", err
	}
	return cardDoc, renderProfile, nil
}

// preflightSummarySchema 与 preflightDocsDisplaySchema 同思路:在 memberCache /
// docsSender / gate 之前独立跑一次 InputSchema 校验(C1 不被绕过)。summary 卡
// 无用户可控 URL 字段,不需要 https 前置校验。
func preflightSummarySchema(card *SummaryCardFields, templateID cardtmpl.ID) error {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}
	tmpl, err := registry.Lookup(templateID, "")
	if err != nil {
		return fmt.Errorf("%w: lookup %s: %v", errCardTmplUnavailable, templateID, err)
	}
	fields, err := mapSummaryCardFieldsToJSON(card)
	if err != nil {
		return fmt.Errorf("%w: map: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	var parsed any
	if err := json.Unmarshal(fields, &parsed); err != nil {
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	if err := tmpl.Meta().InputSchema.Validate(parsed); err != nil {
		cardtmpl.RecordFieldsInvalid(templateID, tmpl.Meta().Version)
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	return nil
}
