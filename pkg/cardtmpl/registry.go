package cardtmpl

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// L2a owner 白名单。L2b (ext.*) 前缀分支在 Register 里保留代码路径,
// 但生产 wiring 只允许注入 L2a owner (§2.2 约束 1;L2b 通道本 PR 不开放)。
var l2aOwnerAllowlist = map[string]struct{}{
	"docs":    {},
	"summary": {},
	"notify":  {},
	"action":  {},
	"ai":      {}, // AI reasoning-process JSON card family (roadmap E1)
}

// L2b owner 前缀。前缀合规但清单不在 docs/l2b-owners.md 里 → Register 拒绝。
const l2bOwnerPrefix = "ext."

// 典型错误。ErrFieldsInvalid 由调用方翻译为 400 (决策 C1);
// 其余属内部错误,调用方按 5xx 或降级策略处理。
var (
	ErrTemplateUnknown = errors.New("cardtmpl: template not registered")
	ErrStateUnknown    = errors.New("cardtmpl: state not declared in manifest")
	ErrFieldsInvalid   = errors.New("cardtmpl: fields did not pass input schema")
	ErrRenderFailed    = errors.New("cardtmpl: render failed post-build")
	ErrRegistryFrozen  = errors.New("cardtmpl: registry frozen")
	ErrActionUnknown   = errors.New("cardtmpl: action not declared by template")
)

// manifestFile 是 Registry 从 assets FS 解析的 manifest.json 形状。
// 与 docs/platform-card-base.md §4.1 对齐;未在 §4.1 声明的字段被忽略。
type manifestFile struct {
	SchemaVersion       int    `json:"schemaVersion"`
	ID                  ID     `json:"id"`
	Name                string `json:"name"`
	Version             string `json:"version"`
	ContractVersion     string `json:"contractVersion"`
	Protocol            string `json:"protocol"`
	AdaptiveCardVersion string `json:"adaptiveCardVersion"`
	DefaultLocale       string `json:"defaultLocale"`
	DataSchema          string `json:"dataSchema"`
	// RenderProfile is provenance-only metadata for the exact Forge artifact.
	// RenderProfileCompatibility is the stable value written to the type-17
	// envelope and is the only render-profile value retained at runtime.
	RenderProfile              string                   `json:"renderProfile"`
	RenderProfileCompatibility string                   `json:"renderProfileCompatibility,omitempty"`
	Owner                      string                   `json:"owner"`
	ActionType                 string                   `json:"actionType"`
	Views                      map[ViewKey]manifestView `json:"views"`
	// SourceLabel / SourceIconURL 可选,若手动指定则覆盖 Template 默认 Source。
	SourceLabel   string `json:"sourceLabel,omitempty"`
	SourceIconURL string `json:"sourceIconUrl,omitempty"`
	// Export is the PR-C D5 opt-in allowlist for B2. Absent means "export no
	// samples", which is the safe default and the permanent answer for frozen
	// static cards.
	Export *exportSamples `json:"export,omitempty"`
}

// manifestView is one entry of manifest.views. Template/Samples are only present
// for JSON-mode cards (roadmap E1): Template is the repo-relative path to the
// view's `.template.json`. Go-mode cards (hand-written Template.Build) omit both
// — the fields are ignored there.
//
// Samples IS the fail-closed allowlist for JSON-mode: LoadJSONBundle reads exactly
// the listed paths, a listed-but-missing sample is a hard registration error, and an
// unlisted `samples/*.json` on disk is never read — so it gets neither schema
// validation nor a render self-check. The manifest, not the directory, is the source
// of truth here; a sample that is not listed silently contributes no coverage.
//
// `goldens/` is asymmetric on purpose: it is still directory-globbed
// (`goldens/*.card.json`, LoadJSONBundle) and is not declared in the manifest, but
// compileBundleGoldens requires every golden to have a same-key sample. Adding
// `goldens/foo.card.json` without also listing the matching sample therefore fails
// registration rather than being ignored.
//
// Go-mode cards keep the older behaviour: Register globs `samples/` via loadSamples
// and ignores this field entirely (PR #654 review P2, PR #670 review).
type manifestView struct {
	WireProfile string   `json:"wireProfile"`
	States      []State  `json:"states"`
	Template    string   `json:"template,omitempty"`
	Samples     []string `json:"samples,omitempty"`
}

// ActionView resolves one declared Action.Submit to its registered view. It is
// used by the action ingress after extracting template identity from the
// effective server-authored frame; OpenUrl and undeclared actions are rejected.
func (r *Registry) ActionView(id ID, version, actionID string) (ViewKey, error) {
	if strings.TrimSpace(actionID) == "" {
		return "", ErrActionUnknown
	}
	e, err := r.entryOf(id, version)
	if err != nil {
		return "", err
	}
	var matched ViewKey
	for view, report := range e.meta.interactions {
		for _, action := range report.Actions {
			if action.Type != "Action.Submit" || action.ID != actionID {
				continue
			}
			if matched != "" && matched != view {
				return "", fmt.Errorf("%w: %s@%s action %q is ambiguous", ErrActionUnknown, id, version, actionID)
			}
			matched = view
		}
	}
	if matched == "" {
		return "", fmt.Errorf("%w: %s@%s action %q", ErrActionUnknown, id, version, actionID)
	}
	return matched, nil
}

// Registry 是进程内模板注册中心。
// - Register 只能在 composition root 期调用;Freeze 之后再 Register/SetDefault → panic;
// - Freeze 之后 Lookup 无锁并发;List / RegisteredForTest 也线程安全。
type Registry struct {
	mu       sync.RWMutex
	frozen   bool
	entries  map[registryKey]*entry
	defaults map[ID]string // id → default version
}

type registryKey struct {
	id      ID
	version string
}

type entry struct {
	tmpl Template
	meta TemplateMeta
	// samples 是注册期从 assets 载入的调用方 fixture, JSON bytes;
	// 用于 Register 期的一致性 self-check 与 conformance test。
	samples map[string]json.RawMessage
}

// NewRegistry 构造空注册表。composition root 之后立即 Freeze()。
func NewRegistry() *Registry {
	return &Registry{
		entries:  make(map[registryKey]*entry),
		defaults: make(map[ID]string),
	}
}

// Register 载入 assets 下的模板契约并注册 t。任何一致性问题 → panic (fail-close)。
//   - <root>/manifest.json 必须存在;views/states 完整;owner+actionType 与 t.Meta 一致;
//   - <root>/contract/data.schema.json 必须能 Compile 通过;
//   - 每个 v2 view 必须有对应 <root>/reports/<view>.interaction.json;
//   - <root>/samples/*.json 全部通过 schema 校验;
//   - owner 必须在 L2a allowlist 内 (L2b 未开放,前缀 ext.* 也拒绝)。
//
// Register 期 t 只提供未装配 Meta 的骨架实例;真正的 TemplateMeta 由 Register 组装并
// 用 setMeta 注入回 t (通过运行时接口 metaSetter,由 pilot Template 实现)。
func (r *Registry) Register(t Template, assets embed.FS, root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Errorf("%w: cannot Register after Freeze", ErrRegistryFrozen))
	}

	mf := loadManifest(assets, root)
	loadedSchema := loadSchema(assets, root)
	if _, hasJSONOnlyConstraints := loadedSchema.document["x-octo-constraints"]; hasJSONOnlyConstraints {
		panic(fmt.Errorf("cardtmpl: Go-authored template %s@%s cannot declare x-octo-constraints; use RegisterJSON", mf.ID, mf.Version))
	}
	schema := loadedSchema.schema
	interactions := loadInteractionReports(assets, root, mf.Views)
	samples := loadSamples(assets, root, schema)

	meta, err := assembleTemplateMeta(mf, schema, interactions, manifestBytes(assets, root))
	if err != nil {
		panic(fmt.Errorf("cardtmpl: register %s: %w", root, err))
	}
	// PR-C D5: the B2 projection is built here, from the same reviewed bytes
	// the template itself was built from, and never at request time. A static
	// L1 card cannot declare an export sample allowlist — its manifest is
	// frozen — so its exported sample set is permanently empty.
	export, err := buildSafeExport(meta, "", CatalogVisibilityPrivate, mf.Owner,
		rawSchemaBytes(assets, root), rawInteractionReports(assets, root, mf.Views), nil, nil)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: register export projection %s: %w", root, err))
	}
	meta.export = export

	// 注入 Meta 到 Template
	setter, ok := t.(metaSetter)
	if !ok {
		panic(fmt.Errorf("cardtmpl: Template %T must implement metaSetter (SetMeta(TemplateMeta))", t))
	}
	setter.SetMeta(meta)

	key := registryKey{id: meta.ID, version: meta.Version}
	if _, dup := r.entries[key]; dup {
		panic(fmt.Errorf("cardtmpl: duplicate registration %s@%s", meta.ID, meta.Version))
	}
	r.entries[key] = &entry{tmpl: t, meta: meta, samples: samples}

	// Sample self-check (§5 约束 3 + brief A8/A15/A16 硬化):
	// 每份 sample 走一次 Render,断言:
	//   1) Render 无错(schema 通过 sample 已在 loadSamples 保证;这里再验 view/Build 联动);
	//   2) 若 Meta.ActionContract 非空,产物里每个 Action.Submit.data.{owner,action_type}
	//      必须与 ActionContract 严格一致 —— 防止 owner 值散布多处飘移,把"code-vs-report
	//      三方一致性"锁到注册期而非只在 CI test 里(生产 boot 就 fail-close)。
	// selfCheckEnv 用最小合法值(https origin, zh-CN, space="__selfcheck__"),
	// deep-link 拼接对 caller 输入无副作用。
	if err := r.selfCheckSamples(t, meta, samples); err != nil {
		panic(fmt.Errorf("cardtmpl: register self-check %s@%s: %w", meta.ID, meta.Version, err))
	}
}

func (r *Registry) registerCompiledJSON(artifact *CompiledArtifact) {
	if artifact == nil || artifact.Template == nil {
		panic(errors.New("cardtmpl: RegisterJSON received nil compiled artifact"))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Errorf("%w: cannot RegisterJSON after Freeze", ErrRegistryFrozen))
	}
	meta := artifact.Meta.Clone()
	key := registryKey{id: meta.ID, version: meta.Version}
	if _, duplicate := r.entries[key]; duplicate {
		panic(fmt.Errorf("cardtmpl: duplicate registration %s@%s", meta.ID, meta.Version))
	}
	r.entries[key] = &entry{
		tmpl:    artifact.Template,
		meta:    meta,
		samples: cloneRawMessageMap(artifact.samples),
	}
}

func assembleTemplateMeta(
	mf manifestFile,
	schema *jsonschema.Schema,
	interactions map[ViewKey]InteractionReport,
	manifest json.RawMessage,
) (TemplateMeta, error) {
	stateIndex := make(map[State]ViewKey, 8)
	views := make(map[ViewKey]ViewSpec, len(mf.Views))
	for view, spec := range mf.Views {
		views[view] = ViewSpec{WireProfile: spec.WireProfile, States: append([]State(nil), spec.States...)}
		for _, state := range spec.States {
			if previous, duplicate := stateIndex[state]; duplicate {
				return TemplateMeta{}, fmt.Errorf("state %q declared in multiple views (%q and %q)", state, previous, view)
			}
			stateIndex[state] = view
		}
	}

	if mf.Owner != "" {
		if strings.HasPrefix(mf.Owner, l2bOwnerPrefix) {
			return TemplateMeta{}, fmt.Errorf("L2b owner %q rejected: ext.* channel not enabled (see docs/platform-card-base.md §2.2-5)", mf.Owner)
		}
		if _, ok := l2aOwnerAllowlist[mf.Owner]; !ok {
			return TemplateMeta{}, fmt.Errorf("owner %q not in L2a allowlist", mf.Owner)
		}
	}

	protocol := mf.Protocol
	if protocol == "" {
		protocol = Protocol
	}
	if protocol != Protocol {
		return TemplateMeta{}, fmt.Errorf("manifest protocol %q != base protocol %q", protocol, Protocol)
	}
	renderProfileCompatibility := strings.TrimSpace(mf.RenderProfileCompatibility)
	if !cardmsg.IsAcceptedRenderProfile(renderProfileCompatibility) {
		return TemplateMeta{}, fmt.Errorf("unsupported render profile compatibility %q", renderProfileCompatibility)
	}
	if renderProfileCompatibility != "" && strings.TrimSpace(mf.RenderProfile) == "" {
		return TemplateMeta{}, errors.New("renderProfile is required when renderProfileCompatibility is set")
	}

	var contract *TemplateActionContract
	if mf.Owner != "" && mf.ActionType != "" {
		contract = &TemplateActionContract{Owner: mf.Owner, ActionType: mf.ActionType}
	}
	return TemplateMeta{
		ID:                         mf.ID,
		Version:                    mf.Version,
		Protocol:                   protocol,
		RenderProfileCompatibility: renderProfileCompatibility,
		Views:                      views,
		ActionContract:             contract,
		Manifest:                   append(json.RawMessage(nil), manifest...),
		InputSchema:                schema,
		Source:                     Source{Label: mf.SourceLabel, IconURL: mf.SourceIconURL},
		stateIndex:                 stateIndex,
		interactions:               interactions,
		contractVersion:            mf.ContractVersion,
	}, nil
}

// SetDefault 显式设置某 id 的默认版本。Freeze 之后调用 → panic。
func (r *Registry) SetDefault(id ID, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Errorf("%w: cannot SetDefault after Freeze", ErrRegistryFrozen))
	}
	if _, ok := r.entries[registryKey{id: id, version: version}]; !ok {
		panic(fmt.Errorf("cardtmpl: SetDefault: %s@%s not registered", id, version))
	}
	r.defaults[id] = version
}

// Freeze 冻结注册表。之后 Register/SetDefault → panic;Lookup 无锁并发。
func (r *Registry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Lookup 查找模板。version=="" 走 SetDefault;未命中返 ErrTemplateUnknown。
func (r *Registry) Lookup(id ID, version string) (Template, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if version == "" {
		v, ok := r.defaults[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s (no default set)", ErrTemplateUnknown, id)
		}
		version = v
	}
	e, ok := r.entries[registryKey{id: id, version: version}]
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", ErrTemplateUnknown, id, version)
	}
	return e.tmpl, nil
}

// List 导出已注册模板元数据 (稳定顺序,按 id@version 升序),供 /v1/message/card/templates 使用。
// R2-6: 深拷贝 Views / stateIndex / interactions / *ActionContract / Manifest bytes,
// 保证外部持有者(如 HTTP handler 或 test)修改返值不会影响 Registry 内部 (frozen 后
// 契约仍不可变)。热路径 Render 走 renderCore 直接读 e.meta 不经过 List,不受影响。
func (r *Registry) List() []TemplateMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TemplateMeta, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.meta.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// RegisteredForTest 返回与生产相同的注册集合,供 conformance test 用。
func (r *Registry) RegisteredForTest() []Template {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Template, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.tmpl)
	}
	return out
}

// sampleFor 返回指定 id@version 的 sample bytes,给 conformance test 用。
func (r *Registry) sampleFor(id ID, version, name string) (json.RawMessage, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[registryKey{id: id, version: version}]
	if !ok {
		return nil, false
	}
	s, ok := e.samples[name]
	return s, ok
}

// samplesFor 返回指定 id@version 的所有 sample,map[name]bytes,给 conformance test 用。
func (r *Registry) samplesFor(id ID, version string) map[string]json.RawMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[registryKey{id: id, version: version}]
	if !ok {
		return nil
	}
	// 拷贝防止调用方修改
	out := make(map[string]json.RawMessage, len(e.samples))
	for k, v := range e.samples {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

// metaSetter 允许 Registry 在 Register 期把组装好的 TemplateMeta 注回 Template 实现。
// 每张 pilot Template 实现该接口 (指针接收者),Meta() 之后返回同一实例。
type metaSetter interface {
	SetMeta(TemplateMeta)
}

// ---- asset loaders (panic on any error;Register 阶段 fail-close) ----

func loadManifest(fs embed.FS, root string) manifestFile {
	b, err := fs.ReadFile(path.Join(root, "manifest.json"))
	if err != nil {
		panic(fmt.Errorf("cardtmpl: read manifest.json in %s: %w", root, err))
	}
	var mf manifestFile
	if err := json.Unmarshal(b, &mf); err != nil {
		panic(fmt.Errorf("cardtmpl: parse manifest.json in %s: %w", root, err))
	}
	if strings.TrimSpace(string(mf.ID)) == "" || strings.TrimSpace(mf.Version) == "" {
		panic(fmt.Errorf("cardtmpl: manifest.json in %s missing id/version", root))
	}
	if len(mf.Views) == 0 {
		panic(fmt.Errorf("cardtmpl: manifest.json in %s has no views", root))
	}
	return mf
}

func manifestBytes(fs embed.FS, root string) json.RawMessage {
	b, _ := fs.ReadFile(path.Join(root, "manifest.json"))
	return append(json.RawMessage(nil), b...)
}

type loadedSchema struct {
	schema   *jsonschema.Schema
	document map[string]any
}

func loadSchema(fs embed.FS, root string) loadedSchema {
	p := path.Join(root, "contract", "data.schema.json")
	b, err := fs.ReadFile(p)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: read %s: %w", p, err))
	}
	parsed, err := decodeStrictJSON(b, jsonBudget{maxDepth: maxArtifactJSONDepth, maxNodes: maxArtifactJSONNodes})
	if err != nil {
		panic(fmt.Errorf("cardtmpl: parse schema %s: %w", p, err))
	}
	document, ok := parsed.(map[string]any)
	if !ok {
		panic(fmt.Errorf("cardtmpl: schema %s root must be an object", p))
	}
	sch, err := compileJSONSchema(document)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: compile schema %s: %w", p, err))
	}
	return loadedSchema{schema: sch, document: document}
}

func loadInteractionReports(fs embed.FS, root string, views map[ViewKey]manifestView) map[ViewKey]InteractionReport {
	out := make(map[ViewKey]InteractionReport)
	for vk, vs := range views {
		if vs.WireProfile != profileV2 {
			continue // 仅 v2 视图需要 interaction report
		}
		p := path.Join(root, "reports", string(vk)+".interaction.json")
		b, err := fs.ReadFile(p)
		if err != nil {
			// v2 view 声明就必须提供 interaction report。缺失 → 注册期 panic (fail-close),
			// 与 docs/platform-card-base.md §5 约束 3 一致 —— 交互契约是本档的强制机读锁,
			// 允许"占位式声明"会导致 Register 通过、Render 到该 view 时才失败。
			// 需要占位视图? 不要在 manifest.views 里声明它。
			panic(fmt.Errorf("cardtmpl: v2 view %q declared in %s/manifest.json but %s missing: %w",
				vk, root, p, err))
		}
		var rep InteractionReport
		if err := json.Unmarshal(b, &rep); err != nil {
			panic(fmt.Errorf("cardtmpl: parse %s: %w", p, err))
		}
		out[vk] = rep
	}
	return out
}

func loadSamples(fs embed.FS, root string, schema *jsonschema.Schema) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	entries, err := fs.ReadDir(path.Join(root, "samples"))
	if err != nil {
		return out // samples 目录可选
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := path.Join(root, "samples", e.Name())
		b, err := fs.ReadFile(p)
		if err != nil {
			panic(fmt.Errorf("cardtmpl: read %s: %w", p, err))
		}
		if err := schema.Validate(mustParseJSON(b)); err != nil {
			panic(fmt.Errorf("cardtmpl: sample %s does not pass schema: %w", p, err))
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		out[name] = append(json.RawMessage(nil), b...)
	}
	return out
}

func mustParseJSON(b []byte) any {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		panic(fmt.Errorf("cardtmpl: parse JSON: %w", err))
	}
	return v
}

// forEachEntry 遍历所有已注册 entry,供 render/metrics 内部使用 (非并发)。
func (r *Registry) forEachEntry(fn func(k registryKey, e *entry)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, e := range r.entries {
		fn(k, e)
	}
}

// entryOf 内部单入口访问 entry,Render 用。
func (r *Registry) entryOf(id ID, version string) (*entry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if version == "" {
		v, ok := r.defaults[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s (no default set)", ErrTemplateUnknown, id)
		}
		version = v
	}
	e, ok := r.entries[registryKey{id: id, version: version}]
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", ErrTemplateUnknown, id, version)
	}
	return e, nil
}

// selfCheckSamples 在 Register 期用每份 sample 跑一次 Render(不走 mu 加锁,因为
// 调用方已在 mu.Lock 内),对 v2 视图断言:
//   - Render 无错(与 cardmsg.Validate 联动);
//   - 若 Meta.ActionContract 非空,产物里每个 Action.Submit.data.{owner,action_type}
//     必须与 ActionContract 严格一致。
//
// 该 self-check 只 covering 有 sample 的 view;没 sample 的 view 由 conformance test
// 或运行期 Render 自然捕获。
func (r *Registry) selfCheckSamples(t Template, meta TemplateMeta, samples map[string]json.RawMessage) error {
	// 反向索引:name → state (samples 命名与 state 值同名,例 pending.json → State("pending"))
	for name, raw := range samples {
		st := State(name)
		view, ok := meta.stateIndex[st]
		if !ok {
			continue // sample 没对应 state 声明,跳过(loader 已对齐 schema,这里只做 render 验证)
		}
		vs, hasView := meta.Views[view]
		if !hasView || vs.WireProfile != profileV2 {
			continue // 非 v2 view 无 interaction 契约,不需要 self-check owner/action_type
		}
		env := BuildEnv{
			WebLoginURL: "https://selfcheck.internal",
			Lang:        "zh-CN",
			SpaceID:     "__cardtmpl_selfcheck__",
		}
		payload, err := r.renderWithinLock(t, meta, st, raw, env)
		if err != nil {
			return fmt.Errorf("sample %s (state=%s, view=%s) render: %w", name, st, view, err)
		}
		if meta.ActionContract != nil {
			if err := assertActionContract(payload, *meta.ActionContract); err != nil {
				return fmt.Errorf("sample %s: %w", name, err)
			}
		}
		report, ok := meta.Interaction(view)
		if !ok {
			return fmt.Errorf("sample %s: interaction report missing for v2 view %s", name, view)
		}
		if err := assertInteractionReport(payload, report); err != nil {
			return fmt.Errorf("sample %s: %w", name, err)
		}
	}
	return nil
}

// renderWithinLock 是 selfCheckSamples 用的内部渲染入口,避免 Render 内部再走 entryOf
// (需要 mu.RLock,而 Register 已 mu.Lock,会 deadlock)。逻辑与 Render 一致但直接
// 拿 t 和 meta,不 Lookup。R2-7: 注册期无 caller ctx,传 context.Background()。
func (r *Registry) renderWithinLock(
	t Template, meta TemplateMeta,
	state State, fields json.RawMessage, env BuildEnv,
) (map[string]any, error) {
	// 复用 Render 的核心;把 entry 已知的部分直接传入,避免 lookup。
	return renderCore(context.Background(), t, meta, state, fields, env)
}

// assertActionContract checks both top-level actions and inline ActionSets.
// Forge-aligned layouts place their production actions in a full-bleed footer,
// so checking only card.actions would leave the registration fail-close gate
// blind to the actual interactive surface.
func assertActionContract(payload map[string]any, contract TemplateActionContract) error {
	card, _ := payload["card"].(map[string]any)
	if card == nil {
		return errors.New("card missing from payload")
	}
	seenSubmit := 0
	check := func(path string, action map[string]any) error {
		seenSubmit++
		data, _ := action["data"].(map[string]any)
		if data == nil {
			return fmt.Errorf("%s Action.Submit missing data", path)
		}
		if owner, _ := data["owner"].(string); owner != contract.Owner {
			return fmt.Errorf("%s data.owner=%q, want %q", path, owner, contract.Owner)
		}
		if actionType, _ := data["action_type"].(string); actionType != contract.ActionType {
			return fmt.Errorf("%s data.action_type=%q, want %q", path, actionType, contract.ActionType)
		}
		return nil
	}
	for _, root := range []struct {
		path  string
		value any
	}{
		{path: "card.actions", value: card["actions"]},
		{path: "card.body", value: card["body"]},
		{path: "card.selectAction", value: card["selectAction"]},
	} {
		if err := walkSubmitActions(root.value, root.path, check); err != nil {
			return err
		}
	}
	if seenSubmit == 0 {
		return errors.New("ActionContract non-nil but no Action.Submit in card")
	}
	return nil
}

func walkSubmitActions(value any, path string, visit func(string, map[string]any) error) error {
	switch node := value.(type) {
	case []any:
		for i, child := range node {
			if err := walkSubmitActions(child, fmt.Sprintf("%s[%d]", path, i), visit); err != nil {
				return err
			}
		}
	case map[string]any:
		if node["type"] == "Action.Submit" {
			if err := visit(path, node); err != nil {
				return err
			}
		}
		for _, key := range []string{"body", "actions", "items", "columns", "rows", "cells", "selectAction", "inlineAction"} {
			if child, ok := node[key]; ok {
				if err := walkSubmitActions(child, path+"."+key, visit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// rawSchemaBytes returns the reviewed data-schema document verbatim. B2 ships
// this copy rather than re-serializing the compiled schema, because a
// round-trip through the compiler loses annotations, examples and key order
// that a producer reads the schema for.
func rawSchemaBytes(assets embed.FS, root string) json.RawMessage {
	raw, err := assets.ReadFile(path.Join(root, "contract", "data.schema.json"))
	if err != nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// rawInteractionReports returns the v2 views' interaction reports verbatim.
// loadInteractionReports already proved each one exists and parses, so a read
// failure here can only mean the embedded FS changed underneath us.
func rawInteractionReports(assets embed.FS, root string, views map[ViewKey]manifestView) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(views))
	for view, spec := range views {
		if spec.WireProfile != profileV2 {
			continue
		}
		raw, err := assets.ReadFile(path.Join(root, "reports", string(view)+".interaction.json"))
		if err != nil {
			continue
		}
		out[string(view)] = append(json.RawMessage(nil), raw...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetCatalogVisibility marks a registered static template as discoverable by
// callers outside its owning Space. It must run before Freeze.
//
// PR-C D5 puts this at the composition root rather than in the template's own
// manifest for two reasons: an L1 manifest is frozen at publish and cannot grow
// a field, and "who may see this card" is a deployment decision that deserves
// to be reviewable in one place rather than spread across template packages.
// Every static template is private until named here, so a card becomes visible
// only by an explicit, reviewed edit — never by an omission.
func (r *Registry) SetCatalogVisibility(id ID, version, visibility string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Errorf("%w: cannot SetCatalogVisibility after Freeze", ErrRegistryFrozen))
	}
	if visibility != CatalogVisibilityPublic && visibility != CatalogVisibilityPrivate {
		panic(fmt.Errorf("cardtmpl: unsupported catalog visibility %q for %s@%s", visibility, id, version))
	}
	e, ok := r.entries[registryKey{id: id, version: version}]
	if !ok {
		panic(fmt.Errorf("cardtmpl: SetCatalogVisibility for unregistered %s@%s", id, version))
	}
	if e.meta.export == nil {
		panic(fmt.Errorf("cardtmpl: %s@%s has no export projection to publish", id, version))
	}
	updated := e.meta.export.Clone()
	updated.Visibility = visibility
	hash, _, err := hashSafeExport(updated)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: rehash export projection %s@%s: %w", id, version, err))
	}
	updated.Hash = hash
	e.meta.export = updated
}

// ExportFor returns the B2 projection for one frozen template, or nil when the
// template is unknown.
//
// It reads the registry entry rather than Template.Meta() on purpose: a
// Template holds the copy of its metadata that was injected at registration,
// and SetCatalogVisibility deliberately does not push a new copy into it (not
// every Template implementation accepts one). The registry entry is the single
// place where the current projection lives.
func (r *Registry) ExportFor(id ID, version string) *SafeExport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if version == "" {
		v, ok := r.defaults[id]
		if !ok {
			return nil
		}
		version = v
	}
	e, ok := r.entries[registryKey{id: id, version: version}]
	if !ok {
		return nil
	}
	return e.meta.export.Clone()
}

// StaticCatalogEntry is one frozen template as the discovery layer sees it.
type StaticCatalogEntry struct {
	ID              ID
	Version         string
	Owner           string
	Protocol        string
	ContractVersion string
	Visibility      string
	ActionContract  *TemplateActionContract
	ExportHash      string
	// IsDefault reports whether this exact version is the ID's registered
	// default, which is what a static new send would resolve to.
	IsDefault bool
}

// StaticCatalog lists the frozen templates for B1. It returns metadata only:
// the projection itself is fetched per template by B2, so a list response never
// carries manifest or schema bytes.
func (r *Registry) StaticCatalog() []StaticCatalogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StaticCatalogEntry, 0, len(r.entries))
	for key, e := range r.entries {
		entry := StaticCatalogEntry{
			ID: key.id, Version: key.version,
			Protocol:        e.meta.Protocol,
			ContractVersion: e.meta.contractVersion,
			Visibility:      CatalogVisibilityPrivate,
			IsDefault:       r.defaults[key.id] == key.version,
		}
		if e.meta.ActionContract != nil {
			contract := *e.meta.ActionContract
			entry.ActionContract = &contract
			entry.Owner = contract.Owner
		}
		if e.meta.export != nil {
			entry.Visibility = e.meta.export.Visibility
			entry.ExportHash = e.meta.export.Hash
			if entry.Owner == "" {
				entry.Owner = e.meta.export.Owner
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return out
}
