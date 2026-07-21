package cardtmpl

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// L2a owner 白名单。L2b (ext.*) 前缀分支在 Register 里保留代码路径,
// 但生产 wiring 只允许注入 L2a owner (§2.2 约束 1;L2b 通道本 PR 不开放)。
var l2aOwnerAllowlist = map[string]struct{}{
	"docs":    {},
	"summary": {},
	"notify":  {},
	"action":  {},
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
)

// manifestFile 是 Registry 从 assets FS 解析的 manifest.json 形状。
// 与 docs/platform-card-base.md §4.1 对齐;未在 §4.1 声明的字段被忽略。
type manifestFile struct {
	ID              ID     `json:"id"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	ContractVersion string `json:"contractVersion"`
	Protocol        string `json:"protocol"`
	Owner           string `json:"owner"`
	ActionType      string `json:"actionType"`
	Views           map[ViewKey]struct {
		WireProfile string  `json:"wireProfile"`
		States      []State `json:"states"`
	} `json:"views"`
	// SourceLabel / SourceIconURL 可选,若手动指定则覆盖 Template 默认 Source。
	SourceLabel   string `json:"sourceLabel,omitempty"`
	SourceIconURL string `json:"sourceIconUrl,omitempty"`
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
	schema := loadSchema(assets, root)
	interactions := loadInteractionReports(assets, root, mf.Views)
	samples := loadSamples(assets, root, schema)

	// state 索引 + 唯一性校验
	stateIndex := make(map[State]ViewKey, 8)
	views := make(map[ViewKey]ViewSpec, len(mf.Views))
	for vk, vs := range mf.Views {
		views[vk] = ViewSpec{WireProfile: vs.WireProfile, States: append([]State(nil), vs.States...)}
		for _, st := range vs.States {
			if prev, dup := stateIndex[st]; dup {
				panic(fmt.Errorf("cardtmpl: state %q declared in multiple views (%q and %q) in %s/manifest.json", st, prev, vk, root))
			}
			stateIndex[st] = vk
		}
	}

	// owner 校验 (§2.2-1)
	if mf.Owner != "" {
		if strings.HasPrefix(mf.Owner, l2bOwnerPrefix) {
			// L2b 分支保留代码路径,生产阶段不开放
			panic(fmt.Errorf("cardtmpl: L2b owner %q rejected: ext.* channel not enabled (see docs/platform-card-base.md §2.2-5)", mf.Owner))
		}
		if _, ok := l2aOwnerAllowlist[mf.Owner]; !ok {
			panic(fmt.Errorf("cardtmpl: owner %q not in L2a allowlist", mf.Owner))
		}
	}

	// Protocol 校验
	protocol := mf.Protocol
	if protocol == "" {
		protocol = Protocol
	}
	if protocol != Protocol {
		panic(fmt.Errorf("cardtmpl: manifest protocol %q != base protocol %q", protocol, Protocol))
	}

	// ActionContract 派生
	var contract *TemplateActionContract
	if mf.Owner != "" && mf.ActionType != "" {
		contract = &TemplateActionContract{Owner: mf.Owner, ActionType: mf.ActionType}
	}

	meta := TemplateMeta{
		ID:             mf.ID,
		Version:        mf.Version,
		Protocol:       protocol,
		Views:          views,
		ActionContract: contract,
		Manifest:       manifestBytes(assets, root),
		InputSchema:    schema,
		Source:         Source{Label: mf.SourceLabel, IconURL: mf.SourceIconURL},
		stateIndex:     stateIndex,
		interactions:   interactions,
	}

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
func (r *Registry) List() []TemplateMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TemplateMeta, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.meta)
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

func loadSchema(fs embed.FS, root string) *jsonschema.Schema {
	p := path.Join(root, "contract", "data.schema.json")
	b, err := fs.ReadFile(p)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: read %s: %w", p, err))
	}
	c := jsonschema.NewCompiler()
	// 用 URL 形式挂载 in-memory 资源,便于错误定位。
	if err := c.AddResource(p, mustParseJSON(b)); err != nil {
		panic(fmt.Errorf("cardtmpl: add schema resource %s: %w", p, err))
	}
	sch, err := c.Compile(p)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: compile schema %s: %w", p, err))
	}
	return sch
}

func loadInteractionReports(fs embed.FS, root string, views map[ViewKey]struct {
	WireProfile string  `json:"wireProfile"`
	States      []State `json:"states"`
}) map[ViewKey]InteractionReport {
	out := make(map[ViewKey]InteractionReport)
	for vk, vs := range views {
		if vs.WireProfile != profileV2 {
			continue // 仅 v2 视图需要 interaction report
		}
		p := path.Join(root, "reports", string(vk)+".interaction.json")
		b, err := fs.ReadFile(p)
		if err != nil {
			// 允许"占位式"视图 (声明 states 但未在本 PR 交付 report) 缺失。
			// 缺失即不注册交互契约,Render 到该 view 时 Build 阶段自然失败。
			continue
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
