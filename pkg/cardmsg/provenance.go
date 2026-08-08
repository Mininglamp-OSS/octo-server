package cardmsg

// PR-C D3（spec: .octospec/tasks/cardtmpl-runtime-catalog-grants-discovery/
// brief.md）：type-17 信封的 server-authored 顶层 catalog 标记。
//
// 两个标记都只能由可信边界写入 —— Bot Registry 路径（authBot 身份）与
// internal carddispatch（已注册 ProducerSpec）；任何 raw ingress
// （bot raw / robot legacy / user / incoming-webhook）必须显式拒绝携带它们。
// 这里只定义 wire 形状与 strict parse：畸形标记是篡改信号，一律返回错误，
// 绝不退化成 legacy 空标记（与 CardTemplateContext 的 PR#641 fail-close
// 口径一致）。principal 与授权语义在 pkg/cardtmpl 层（cardmsg 不做授权）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// CatalogProvenanceKey / CatalogTemplateRefKey 是 type-17 信封的
	// server-only 顶层键。raw ingress 检测/拒绝时统一引用这两个常量。
	CatalogProvenanceKey  = "catalog_provenance"
	CatalogTemplateRefKey = "template_ref"

	// CatalogProvenanceVersion 是 v1 标记版本；未来演进 bump 该值并保持
	// 旧版本可解析（额外形状仍然 strict）。
	CatalogProvenanceVersion = 1

	// Wire principal 类型。只有可认证的发送主体能成为 wire principal；
	// space 是 discover-only grant principal（D2），system 是内部保留值，
	// 二者都不会出现在存储帧里。
	CatalogPrincipalWireBot              = "bot"
	CatalogPrincipalWireInternalProducer = "internal_producer"
)

// ErrCatalogMarkerInvalid 标识畸形/不一致的 server-authored catalog 标记。
var ErrCatalogMarkerInvalid = errors.New("cardmsg: invalid catalog marker")

// CatalogProvenance 是 catalog_provenance 顶层标记的 typed 值对象。
// 字段与 wire 形状一一对应；SpaceID 允许为空（发送边界没有权威 Space 的
// 频道形态，如 Slice 3 之前的群聊 Bot 发送），非空值必须已 trim。
type CatalogProvenance struct {
	Version       int
	PrincipalType string
	PrincipalID   string
	SpaceID       string
}

// Validate 校验 canonical v1 形状。
func (p CatalogProvenance) Validate() error {
	if p.Version != CatalogProvenanceVersion {
		return fmt.Errorf("%w: unsupported provenance version %d", ErrCatalogMarkerInvalid, p.Version)
	}
	if p.PrincipalType != CatalogPrincipalWireBot && p.PrincipalType != CatalogPrincipalWireInternalProducer {
		return fmt.Errorf("%w: unsupported principal_type %q", ErrCatalogMarkerInvalid, p.PrincipalType)
	}
	if p.PrincipalID == "" || strings.TrimSpace(p.PrincipalID) != p.PrincipalID {
		return fmt.Errorf("%w: principal_id is required and must be trimmed", ErrCatalogMarkerInvalid)
	}
	if strings.TrimSpace(p.SpaceID) != p.SpaceID {
		return fmt.Errorf("%w: space_id must be trimmed", ErrCatalogMarkerInvalid)
	}
	return nil
}

// MarshalMap 输出 canonical wire map。key 集合与 ParseCatalogProvenance 的
// strict 校验互为镜像；新增字段必须同时改两处并 bump version 语义评审。
func (p CatalogProvenance) MarshalMap() map[string]interface{} {
	return map[string]interface{}{
		"version":        p.Version,
		"principal_type": p.PrincipalType,
		"principal_id":   p.PrincipalID,
		"space_id":       p.SpaceID,
	}
}

// ParseCatalogProvenance strict 解析一个 catalog_provenance 值。未知键、
// 缺键、类型错误、未知 principal、未 trim 值全部拒绝。
func ParseCatalogProvenance(value interface{}) (CatalogProvenance, error) {
	object, ok := value.(map[string]interface{})
	if !ok || object == nil {
		return CatalogProvenance{}, fmt.Errorf("%w: catalog_provenance must be an object", ErrCatalogMarkerInvalid)
	}
	allowed := map[string]struct{}{"version": {}, "principal_type": {}, "principal_id": {}, "space_id": {}}
	for key := range object {
		if _, known := allowed[key]; !known {
			return CatalogProvenance{}, fmt.Errorf("%w: unknown catalog_provenance key %q", ErrCatalogMarkerInvalid, key)
		}
	}
	for key := range allowed {
		if _, present := object[key]; !present {
			return CatalogProvenance{}, fmt.Errorf("%w: catalog_provenance missing key %q", ErrCatalogMarkerInvalid, key)
		}
	}
	version, ok := provenanceVersion(object["version"])
	if !ok {
		return CatalogProvenance{}, fmt.Errorf("%w: catalog_provenance version is not an integer", ErrCatalogMarkerInvalid)
	}
	principalType, ok := object["principal_type"].(string)
	if !ok {
		return CatalogProvenance{}, fmt.Errorf("%w: principal_type must be a string", ErrCatalogMarkerInvalid)
	}
	principalID, ok := object["principal_id"].(string)
	if !ok {
		return CatalogProvenance{}, fmt.Errorf("%w: principal_id must be a string", ErrCatalogMarkerInvalid)
	}
	spaceID, ok := object["space_id"].(string)
	if !ok {
		return CatalogProvenance{}, fmt.Errorf("%w: space_id must be a string", ErrCatalogMarkerInvalid)
	}
	parsed := CatalogProvenance{Version: version, PrincipalType: principalType, PrincipalID: principalID, SpaceID: spaceID}
	if err := parsed.Validate(); err != nil {
		return CatalogProvenance{}, err
	}
	return parsed, nil
}

// provenanceVersion 接受 int / json.Number / 整数值 float64（三种解码路径），
// 拒绝任何有小数部分或非数值的形状。
func provenanceVersion(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int(typed), true
	default:
		return 0, false
	}
}

// CatalogTemplateRef 是顶层 template_ref 的 typed 值对象。
type CatalogTemplateRef struct {
	ID      string
	Version string
}

// ParseCatalogTemplateRef strict 解析顶层 template_ref：恰好 {id, version}
// 两个非空已 trim 字符串键（与 bot_api 的 caller-facing template_ref/v1
// request 形状一致）。
func ParseCatalogTemplateRef(value interface{}) (CatalogTemplateRef, error) {
	object, ok := value.(map[string]interface{})
	if !ok || len(object) != 2 {
		return CatalogTemplateRef{}, fmt.Errorf("%w: template_ref must contain exactly id and version", ErrCatalogMarkerInvalid)
	}
	id, idOK := object["id"].(string)
	version, versionOK := object["version"].(string)
	if !idOK || !versionOK || id == "" || version == "" ||
		strings.TrimSpace(id) != id || strings.TrimSpace(version) != version {
		return CatalogTemplateRef{}, fmt.Errorf("%w: template_ref id/version are required", ErrCatalogMarkerInvalid)
	}
	return CatalogTemplateRef{ID: id, Version: version}, nil
}

// FrameCatalogMarkers 是一个有效帧的 server-authored catalog 身份快照。
type FrameCatalogMarkers struct {
	Ref           CatalogTemplateRef
	Provenance    CatalogProvenance
	HasRef        bool
	HasProvenance bool
}

// CatalogFrameMarkers 从一个有效 type-17 帧提取并校验顶层 catalog 标记。
//
// 兼容矩阵（D3 + invariant 7 static 历史兼容）：
//   - 两个标记都缺失 → legacy 帧，零值返回（不报错）；
//   - 只有 template_ref → pre-PR-C Bot Registry 帧，合法；
//   - 两个都有 → 都 strict 校验；
//   - 只有 catalog_provenance → 服务端从不产出该形状，拒绝；
//   - 任一存在时，template_ref 必须与 metadata.octo.template 完全一致
//     （metadata 缺失/畸形同样拒绝 —— 标记只可能由 Registry 产出，而
//     Registry 必然写 metadata）。
//
// 非 type-17 payload 返回零值（调用方对非卡片本就不该看标记）。
func CatalogFrameMarkers(envelopeRaw []byte) (FrameCatalogMarkers, error) {
	payload, err := decodeEnvelope(string(envelopeRaw))
	if err != nil || !IsCardPayload(payload) {
		return FrameCatalogMarkers{}, nil
	}
	return catalogMarkersFromPayload(payload, envelopeRaw)
}

func catalogMarkersFromPayload(payload map[string]interface{}, envelopeRaw []byte) (FrameCatalogMarkers, error) {
	refValue, hasRef := payload[CatalogTemplateRefKey]
	provenanceValue, hasProvenance := payload[CatalogProvenanceKey]
	if !hasRef && !hasProvenance {
		return FrameCatalogMarkers{}, nil
	}
	if !hasRef {
		return FrameCatalogMarkers{}, fmt.Errorf("%w: catalog_provenance without template_ref", ErrCatalogMarkerInvalid)
	}
	markers := FrameCatalogMarkers{HasRef: true}
	ref, err := ParseCatalogTemplateRef(refValue)
	if err != nil {
		return FrameCatalogMarkers{}, err
	}
	markers.Ref = ref
	if envelopeRaw == nil {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return FrameCatalogMarkers{}, fmt.Errorf("%w: encode envelope: %v", ErrCatalogMarkerInvalid, marshalErr)
		}
		envelopeRaw = encoded
	}
	template, ok := CardTemplateContext(envelopeRaw)
	if !ok || template.ID != ref.ID || template.Version != ref.Version {
		return FrameCatalogMarkers{}, fmt.Errorf("%w: template_ref does not match metadata.octo.template", ErrCatalogMarkerInvalid)
	}
	if hasProvenance {
		provenance, err := ParseCatalogProvenance(provenanceValue)
		if err != nil {
			return FrameCatalogMarkers{}, err
		}
		markers.Provenance = provenance
		markers.HasProvenance = true
	}
	return markers, nil
}

// validateCatalogMarkers 是 Validate 的 defense-in-depth 钩子：任何携带
// catalog 标记的信封（send 或 NormalizeContentEdit 路径）都必须是 canonical
// 形状。raw ingress 的显式拒绝仍在各自边界；这里兜住遗漏的路径。
func validateCatalogMarkers(payload map[string]interface{}) error {
	_, hasRef := payload[CatalogTemplateRefKey]
	_, hasProvenance := payload[CatalogProvenanceKey]
	if !hasRef && !hasProvenance {
		return nil
	}
	_, err := catalogMarkersFromPayload(payload, nil)
	return err
}

// CatalogMarkersPreserved reports whether a replacement frame keeps the catalog
// identity a stored frame carries. It is the single definition of that rule:
// internal/carddispatch's mutation boundary enforces it on every replacement,
// and tests that stand in for that boundary call this rather than restating the
// comparison — an earlier version was hand-copied into a test fake, and the copy
// reproduced a bug in the original instead of catching it.
//
// The rule is asymmetric on purpose, which is the part that was wrong when it
// was written as an equality check:
//
//   - Losing a marker is refused. That is the erasure the boundary exists to
//     stop; both identity guards in pkg/cardtmpl's updater begin `if
//     markers.Has…`, so a dropped marker silently disables them for that
//     message and returns the frame to the legacy population.
//   - Gaining one is allowed. `template_ref` alone is a legal pre-PR-C Registry
//     frame, and an edit is the only moment such a frame can acquire
//     provenance. Refusing that made every already-delivered Registry card
//     permanently uneditable, with no repair path, because the edit that would
//     add the marker was the operation being refused. The gain is safe because
//     only a server rendering boundary can author a marker and only the
//     original sender can reach a mutation.
//   - Changing one is refused. Identity does not change under an edit, so where
//     both sides carry a marker the values must match. Presence alone would let
//     a replacement keep both keys and rewrite principal_id or space_id.
func CatalogMarkersPreserved(stored, next FrameCatalogMarkers) error {
	if stored.HasRef && !next.HasRef {
		return fmt.Errorf("%w: replacement drops the stored template_ref", ErrCatalogMarkerInvalid)
	}
	if stored.HasProvenance && !next.HasProvenance {
		return fmt.Errorf("%w: replacement drops the stored catalog_provenance", ErrCatalogMarkerInvalid)
	}
	if stored.HasRef && next.HasRef && stored.Ref != next.Ref {
		return fmt.Errorf("%w: replacement changes the stored template_ref", ErrCatalogMarkerInvalid)
	}
	if stored.HasProvenance && next.HasProvenance && stored.Provenance != next.Provenance {
		return fmt.Errorf("%w: replacement changes the stored catalog_provenance", ErrCatalogMarkerInvalid)
	}
	return nil
}
