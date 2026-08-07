package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

// profileV1 / profileV2 是 wire profile 枚举,与 pkg/cardmsg 常量同源。
// 本包在多处用到,提取为 package-scoped 常量避免散布字面量。
const (
	profileV1 = cardmsg.ProfileV1
	profileV2 = cardmsg.ProfileV2
)

// Render 是"Template.Build → 完整 InteractiveCard payload" 的唯一入口。
// 业务代码只调 Render;不允许直接调 Template.Build 后自己拼装 payload/metadata。
//
// 流水线 (与 docs/platform-card-base.md §5 对齐):
//  1. Lookup(id, version) 命中 Template + Meta;
//  2. Meta.InputSchema.Validate(fields) + optional JSON-template semantic
//     constraints → 失败返 ErrFieldsInvalid;
//  3. Meta.ViewFor(state) 决定 view/profile → 未注册 state 返 ErrStateUnknown;
//  4. Template.Build(ctx, state, fields, env) 拿 BuildResult;
//  5. BuildResult.DeepLink 可选:非空则校验为绝对 https,空则跳过(无 metadata.webUrl);
//  6. 组装 AC 顶层文档 + 注入 metadata.octo.{protocol, template, variant, source};DeepLink 非空时再注入 metadata.webUrl;
//  7. 构造 InteractiveCard payload envelope (type=17 / card / card_version / profile);
//  8. cardmsg.Validate(payload) → 失败包裹 ErrRenderFailed 返回。
//
// 返回:payload 是完整的 map[string]any (含 type=17 与 card),供调用方按需
// 拆出 payload["card"] 作 carddispatch.Card.Document 使用。
func (r *Registry) Render(
	ctx context.Context,
	id ID,
	version string,
	state State,
	fields json.RawMessage,
	env BuildEnv,
) (map[string]any, error) {
	e, err := r.entryOf(id, version)
	if err != nil {
		metricBuildResult(id, version, "", "template_unknown")
		return nil, err
	}
	return renderCore(ctx, e.tmpl, e.meta, state, fields, env)
}

// renderCore 是 Render 与 Register 期 selfCheckSamples 共享的核心。它接收已解析的
// Template + Meta,不再走 registry 锁 —— 由调用方确保并发安全 (Render 已经 entryOf
// 拿到不可变引用后释放锁;selfCheckSamples 已在 Register 的 mu.Lock 内)。
// R2-7: 显式接收 ctx 并传给 Template.Build,让 Template 能观察调用方的
// cancel/deadline (跨语言模板、慢 build、外部依赖 fetch 等场景)。selfCheckSamples
// 传 context.Background() —— 注册期没有 caller ctx 可传。
func renderCore(
	ctx context.Context,
	t Template, meta TemplateMeta,
	state State, fields json.RawMessage, env BuildEnv,
) (map[string]any, error) {
	// step 2: schema 校验入参
	if len(fields) == 0 {
		metricBuildResult(meta.ID, meta.Version, "", "fields_invalid")
		return nil, fmt.Errorf("%w: fields must not be empty", ErrFieldsInvalid)
	}
	var parsed any
	if err := json.Unmarshal(fields, &parsed); err != nil {
		metricBuildResult(meta.ID, meta.Version, "", "fields_invalid")
		return nil, fmt.Errorf("%w: %v", ErrFieldsInvalid, err)
	}
	if err := meta.InputSchema.Validate(parsed); err != nil {
		metricBuildResult(meta.ID, meta.Version, "", "fields_invalid")
		return nil, fmt.Errorf("%w: %v", ErrFieldsInvalid, err)
	}
	if validator, ok := t.(jsonTemplateFieldValidator); ok {
		if err := validator.validateFields(parsed); err != nil {
			metricBuildResult(meta.ID, meta.Version, "", "fields_invalid")
			return nil, fmt.Errorf("%w: %v", ErrFieldsInvalid, err)
		}
	}

	// step 3: state → view
	view, ok := meta.ViewFor(state)
	if !ok {
		metricBuildResult(meta.ID, meta.Version, "", "state_unknown")
		return nil, fmt.Errorf("%w: state=%q", ErrStateUnknown, state)
	}
	vs, ok := meta.Views[view]
	if !ok {
		// 理论不可达 (ViewFor 与 Views 同源)
		metricBuildResult(meta.ID, meta.Version, string(view), "state_unknown")
		return nil, fmt.Errorf("%w: view %q missing spec", ErrStateUnknown, view)
	}

	// step 4: Build (R2-7 传 caller ctx)
	br, err := t.Build(ctx, state, fields, env)
	if err != nil {
		metricBuildResult(meta.ID, meta.Version, string(view), "render_error")
		// R4-2: 用 %w 包裹底层 err,让 errors.Is 能穿透探到 context.Canceled /
		// DeadlineExceeded(caller 取消/超时);此前 %v 丢链,上游只能看到 ErrRenderFailed。
		return nil, fmt.Errorf("%w: build: %w", ErrRenderFailed, err)
	}

	// step 5: DeepLink 可选;非空则强制绝对 https (§5 约束 4)。空 DeepLink 表示本卡
	// 没有规范浏览器 URL(如 JSON 模板的纯展示进度卡,无 OpenUrl/webUrl)—— 跳过
	// https 校验并在 step 6 省略 metadata.webUrl。既有卡片一律提供 DeepLink,行为不变。
	//
	// "可选" 只放行**真正空**的 DeepLink(""):一个非空但 trim 后为空(纯空白)的
	// DeepLink 是畸形值,走 AbsoluteHTTPSURL 判定为 render_error 而非被静默当作
	// "无链接"。否则 (a) 一张误产出空白链接的 Go 卡会静默丢掉 webUrl,(b) 空白与
	// 真链接在 metric 上无法区分 —— 均破坏 fail-close(PR #654 review)。
	hasDeepLink := br.DeepLink != ""
	if hasDeepLink {
		if err := AbsoluteHTTPSURL(br.DeepLink); err != nil {
			metricBuildResult(meta.ID, meta.Version, string(view), "render_error")
			return nil, fmt.Errorf("%w: DeepLink: %v", ErrRenderFailed, err)
		}
	}

	// step 6: 组装 AC 顶层 + metadata
	src := meta.Source
	if br.Source != nil {
		src = *br.Source
	}
	// P2-2 (#633): source.iconUrl 也走绝对-https 校验后才注入 metadata。cardmsg.Validate
	// 只校验渲染卡节点里的 URL,不看 metadata 字段;pilot 服务端置空所以今天 inert,
	// 但未来模板从较不可信来源填 IconURL 时,不校验会把 javascript:/data:/相对 URL
	// 透到客户端。DeepLink 已在 step5 校验,这里补上 source iconUrl,保持"上线 URL 全校验"。
	if strings.TrimSpace(src.IconURL) != "" {
		if err := AbsoluteHTTPSURL(src.IconURL); err != nil {
			metricBuildResult(meta.ID, meta.Version, string(view), "render_error")
			return nil, fmt.Errorf("%w: source iconUrl: %v", ErrRenderFailed, err)
		}
	}
	octo := map[string]any{
		"protocol": meta.Protocol,
		"template": map[string]any{
			"id":      string(meta.ID),
			"version": meta.Version,
		},
	}
	if strings.TrimSpace(br.Variant) != "" {
		octo["variant"] = br.Variant
	}
	if strings.TrimSpace(src.Label) != "" || strings.TrimSpace(src.IconURL) != "" {
		s := map[string]any{}
		if strings.TrimSpace(src.Label) != "" {
			s["label"] = src.Label
		}
		if strings.TrimSpace(src.IconURL) != "" {
			s["iconUrl"] = src.IconURL
		}
		octo["source"] = s
	}
	metadata := map[string]any{"octo": octo}
	if hasDeepLink {
		metadata["webUrl"] = br.DeepLink
	}
	card := map[string]any{
		"type":     "AdaptiveCard",
		"version":  cardmsg.CardVersion,
		"body":     br.Body,
		"metadata": metadata,
	}
	if len(br.Actions) > 0 {
		card["actions"] = br.Actions
	}

	// step 7: envelope
	payload := map[string]any{
		"type":         int(cardmsg.InteractiveCard),
		"card":         card,
		"card_version": cardmsg.CardVersion,
		"profile":      vs.WireProfile,
	}
	if meta.RenderProfileCompatibility != "" {
		payload["render_profile"] = meta.RenderProfileCompatibility
	}

	// step 8: cardmsg.Validate 一次总校验 (白名单 / 上限 / interaction 门)
	if err := cardmsg.Validate(payload); err != nil {
		metricBuildResult(meta.ID, meta.Version, string(view), "render_error")
		return nil, fmt.Errorf("%w: cardmsg.Validate: %v", ErrRenderFailed, err)
	}
	metricBuildResult(meta.ID, meta.Version, string(view), "ok")
	return payload, nil
}

// RenderCard 是便捷函数:返回 payload["card"] 部分 (裸 AC 文档) 供
// carddispatch.Card.Document 直接使用,同时返回 profile 与完整 payload。
func (r *Registry) RenderCard(
	ctx context.Context,
	id ID,
	version string,
	state State,
	fields json.RawMessage,
	env BuildEnv,
) (cardDoc json.RawMessage, profile string, err error) {
	payload, err := r.Render(ctx, id, version, state, fields, env)
	if err != nil {
		return nil, "", err
	}
	card := payload["card"]
	b, mErr := json.Marshal(card)
	if mErr != nil {
		return nil, "", fmt.Errorf("%w: marshal card: %v", ErrRenderFailed, mErr)
	}
	profile, _ = payload["profile"].(string)
	return b, profile, nil
}

// RenderCardWithProfiles is the envelope-preserving counterpart used by
// internal dispatch/update paths that must carry both the wire capability
// profile and the optional visual compatibility profile.
func (r *Registry) RenderCardWithProfiles(
	ctx context.Context,
	id ID,
	version string,
	state State,
	fields json.RawMessage,
	env BuildEnv,
) (cardDoc json.RawMessage, profile, renderProfile string, err error) {
	payload, err := r.Render(ctx, id, version, state, fields, env)
	if err != nil {
		return nil, "", "", err
	}
	b, marshalErr := json.Marshal(payload["card"])
	if marshalErr != nil {
		return nil, "", "", fmt.Errorf("%w: marshal card: %v", ErrRenderFailed, marshalErr)
	}
	profile, _ = payload["profile"].(string)
	renderProfile, _ = payload["render_profile"].(string)
	return b, profile, renderProfile, nil
}

// AbsoluteHTTPSURL 是本包"必须为绝对 https URL"规则的**唯一真源**。它用真正的
// url.Parse 断言 scheme==https 且 host 非空 —— 正则无法可靠判定 host 存在
// (例 "https://?x" / "https://#x" 的 host 为空却能被 `^https://[^/].*$` 之类
// 的近似 pattern 放行,随后在 Build 内的 requireHTTPS 才失败、被误分类成
// render_error 降级)。需要 C1 级字段校验的调用方 (modules/notify preflight)
// 必须调本函数在 ingress 前置拦截,而不是依赖 schema 的正则近似。
//
// pkg/cardtmpl 内 requireHTTPS (resource.go) 与 renderCore step5 均委托本函数,
// 保证 preflight 通过 → Build 的 https 校验必然通过 (同一实现,零缝隙)。
func AbsoluteHTTPSURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return errors.New("cardtmpl: url must be absolute https, got empty")
	}
	u, err := url.Parse(s)
	// R4-3: 用 u.Hostname()!="" 而非 u.Host!="" —— "https://:443/x" 的 Host 是 ":443"
	// 但 Hostname() 为空(纯端口无主机),旧检查会放过;同时拒绝 userinfo
	// ("https://user@host") 以避免歧义/钓鱼式 URL。本函数是绝对-https 规则唯一真源
	// (requireHTTPS / renderCore 的 DeepLink+source iconUrl 均委托),收紧一次全链受益。
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("cardtmpl: url must be absolute https, got %q", raw)
	}
	return nil
}
