package cardtmpl

import (
	"context"
	"encoding/json"
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
//  2. Meta.InputSchema.Validate(fields) → 失败返 ErrFieldsInvalid;
//  3. Meta.ViewFor(state) 决定 view/profile → 未注册 state 返 ErrStateUnknown;
//  4. Template.Build(ctx, state, fields, env) 拿 BuildResult;
//  5. 校验 BuildResult.DeepLink 为绝对 https;
//  6. 组装 AC 顶层文档 + 注入 metadata.octo.{protocol, template, variant, source} + metadata.webUrl;
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
	return renderCore(e.tmpl, e.meta, state, fields, env)
}

// renderCore 是 Render 与 Register 期 selfCheckSamples 共享的核心。它接收已解析的
// Template + Meta,不再走 registry 锁 —— 由调用方确保并发安全 (Render 已经 entryOf
// 拿到不可变引用后释放锁;selfCheckSamples 已在 Register 的 mu.Lock 内)。
func renderCore(
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

	// step 4: Build
	br, err := t.Build(context.Background(), state, fields, env)
	if err != nil {
		metricBuildResult(meta.ID, meta.Version, string(view), "render_error")
		return nil, fmt.Errorf("%w: build: %v", ErrRenderFailed, err)
	}

	// step 5: DeepLink 强制校验 (§5 约束 4)
	if err := validateHTTPS(br.DeepLink); err != nil {
		metricBuildResult(meta.ID, meta.Version, string(view), "render_error")
		return nil, fmt.Errorf("%w: DeepLink: %v", ErrRenderFailed, err)
	}

	// step 6: 组装 AC 顶层 + metadata
	src := meta.Source
	if br.Source != nil {
		src = *br.Source
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
	card := map[string]any{
		"type":     "AdaptiveCard",
		"version":  cardmsg.CardVersion,
		"body":     br.Body,
		"metadata": map[string]any{"webUrl": br.DeepLink, "octo": octo},
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

// validateHTTPS 强制绝对 https URL,与 pkg/cardtmpl/resource.go:requireHTTPS 语义一致。
func validateHTTPS(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("must be absolute https, got empty")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("must be absolute https, got %q", raw)
	}
	return nil
}
