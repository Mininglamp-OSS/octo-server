package cardtmpl

// PR-C D5 — the safe export projection served by B2.
//
// A template's on-disk or in-bundle form contains things a discovery caller
// must never receive: the view templates themselves, golden files, and (for a
// dynamic artifact) the canonical bytes that hash to its identity. What a
// caller legitimately needs is the *contract* — what states exist, what shape
// the input takes, which actions a view declares, and one or two examples of
// valid input.
//
// The projection is built once, at registration or compile time, and is
// immutable thereafter. Two consequences are deliberate:
//
//   - No request ever reads a source directory, and nothing is reconstructed
//     by inspecting a compiled schema. What ships is a copy of the reviewed
//     bytes, or nothing at all.
//   - The set of exportable documents is fixed when the artifact is built, so a
//     later change to serving code cannot widen it. Widening requires a new
//     artifact version, which goes through publish review.
//
// Samples are the one part a template opts *into*. They are caller-authored
// input fixtures and can easily carry real tenant data, so the default is to
// export none; a bundle must name each exportable sample in its manifest, and
// the compiler rejects a name that does not exist. Static L1 cards are frozen
// and cannot grow that declaration, so their exported sample set is always
// empty — which is the correct answer, not a gap.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// maxSafeExportBytes bounds one serialized projection. It is checked before
// marshalling reaches a response buffer so an oversized artifact fails as an
// integrity problem at build time rather than as a truncated HTTP body.
const maxSafeExportBytes = 2 << 20

// ExportView is one view's contract, without its template document.
type ExportView struct {
	Name        string   `json:"name"`
	WireProfile string   `json:"wire_profile"`
	States      []string `json:"states"`
	// SubmitActions lists the view's declared Action.Submit ids. Present only
	// for interactive views; a display-only view reports an empty list rather
	// than null so callers need no special case.
	SubmitActions []string `json:"submit_actions"`
}

// ExportActionContract is the callback routing a card's Submit actions carry.
// It is nil for a display-only card, which is exactly the signal a producer
// needs to know no callback will ever arrive (platform-card-base §9).
type ExportActionContract struct {
	Owner      string `json:"owner"`
	ActionType string `json:"action_type"`
}

// SafeExport is the immutable projection. Every field on it is safe to return
// to any caller that has already passed the visibility check; there is no
// second filtering step at serving time, because a projection that needed one
// would be a projection that had already been built wrong.
type SafeExport struct {
	ID              string                `json:"id"`
	Version         string                `json:"version"`
	Protocol        string                `json:"protocol"`
	ContractVersion string                `json:"contract_version,omitempty"`
	Owner           string                `json:"owner,omitempty"`
	Engine          string                `json:"engine,omitempty"`
	Visibility      string                `json:"visibility"`
	ActionContract  *ExportActionContract `json:"action_contract"`
	Views           []ExportView          `json:"views"`

	Manifest json.RawMessage            `json:"manifest"`
	Schema   json.RawMessage            `json:"schema,omitempty"`
	Reports  map[string]json.RawMessage `json:"reports,omitempty"`
	// Samples holds only the fixtures the manifest opted into exporting.
	Samples map[string]json.RawMessage `json:"samples"`

	// Hash is a deterministic digest of everything above. Static templates use
	// it as their ETag; dynamic ones use their immutable content hash, which is
	// stronger because it also covers the documents this projection omits.
	Hash string `json:"-"`
}

// exportSamples is the manifest's opt-in allowlist. It is a separate struct so
// the strict unknown-key check in decodeManifest has something concrete to
// validate against.
type exportSamples struct {
	Samples []string `json:"samples"`
}

// buildSafeExport assembles the projection. It returns an error rather than a
// partial result: an artifact whose contract cannot be projected safely is one
// that should fail to compile, not one that should be served with holes.
func buildSafeExport(
	meta TemplateMeta,
	engine, visibility, owner string,
	schema json.RawMessage,
	reports map[string]json.RawMessage,
	samples map[string]json.RawMessage,
	allowlist []string,
) (*SafeExport, error) {
	export := &SafeExport{
		ID:              string(meta.ID),
		Version:         meta.Version,
		Protocol:        meta.Protocol,
		ContractVersion: meta.contractVersion,
		Owner:           owner,
		Engine:          engine,
		Visibility:      normalizeExportVisibility(visibility),
		Views:           make([]ExportView, 0, len(meta.Views)),
		Samples:         map[string]json.RawMessage{},
	}
	if meta.ActionContract != nil {
		export.ActionContract = &ExportActionContract{
			Owner: meta.ActionContract.Owner, ActionType: meta.ActionContract.ActionType,
		}
	}
	for _, name := range sortedViewNames(meta.Views) {
		spec := meta.Views[ViewKey(name)]
		view := ExportView{
			Name: name, WireProfile: spec.WireProfile,
			States:        make([]string, 0, len(spec.States)),
			SubmitActions: []string{},
		}
		for _, state := range spec.States {
			view.States = append(view.States, string(state))
		}
		sort.Strings(view.States)
		if report, ok := meta.Interaction(ViewKey(name)); ok {
			for _, action := range report.Actions {
				if action.Type == "Action.Submit" && action.ID != "" {
					view.SubmitActions = append(view.SubmitActions, action.ID)
				}
			}
			sort.Strings(view.SubmitActions)
		}
		export.Views = append(export.Views, view)
	}
	if meta.Manifest != nil {
		export.Manifest = append(json.RawMessage(nil), meta.Manifest...)
	}
	if schema != nil {
		export.Schema = append(json.RawMessage(nil), schema...)
	}
	if len(reports) > 0 {
		export.Reports = cloneRawMessageMap(reports)
	}
	for _, key := range allowlist {
		raw, ok := samples[key]
		if !ok {
			return nil, fmt.Errorf("export allowlist names sample %q, which does not exist", key)
		}
		export.Samples[key] = append(json.RawMessage(nil), raw...)
	}

	hash, size, err := hashSafeExport(export)
	if err != nil {
		return nil, err
	}
	if size > maxSafeExportBytes {
		return nil, fmt.Errorf("export projection is %d bytes, over the %d byte cap", size, maxSafeExportBytes)
	}
	export.Hash = hash
	return export, nil
}

// hashSafeExport digests the projection through canonical JSON so the value is
// stable across process restarts and across replicas — an ETag that varied by
// map iteration order would silently defeat every conditional request.
func hashSafeExport(export *SafeExport) (string, int, error) {
	encoded, err := json.Marshal(export)
	if err != nil {
		return "", 0, fmt.Errorf("marshal export projection: %w", err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return "", 0, fmt.Errorf("normalize export projection: %w", err)
	}
	canonical, err := marshalCanonicalJSON(normalized)
	if err != nil {
		return "", 0, fmt.Errorf("canonicalize export projection: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), len(canonical), nil
}

// normalizeExportVisibility fails closed. An artifact that never declared its
// visibility is private, because the alternative — treating "unset" as public
// — turns every future omission into a disclosure.
func normalizeExportVisibility(visibility string) string {
	if visibility == CatalogVisibilityPublic {
		return CatalogVisibilityPublic
	}
	return CatalogVisibilityPrivate
}

// Clone returns a deep copy. The projection is immutable by construction, but
// it crosses a package boundary into HTTP handlers, and a handler that mutated
// a shared map would corrupt every later response rather than just its own.
func (e *SafeExport) Clone() *SafeExport {
	if e == nil {
		return nil
	}
	out := *e
	if e.ActionContract != nil {
		contract := *e.ActionContract
		out.ActionContract = &contract
	}
	out.Views = make([]ExportView, len(e.Views))
	for i, view := range e.Views {
		out.Views[i] = ExportView{
			Name: view.Name, WireProfile: view.WireProfile,
			States:        append([]string(nil), view.States...),
			SubmitActions: append([]string(nil), view.SubmitActions...),
		}
	}
	if e.Manifest != nil {
		out.Manifest = append(json.RawMessage(nil), e.Manifest...)
	}
	if e.Schema != nil {
		out.Schema = append(json.RawMessage(nil), e.Schema...)
	}
	out.Reports = cloneRawMessageMap(e.Reports)
	out.Samples = cloneRawMessageMap(e.Samples)
	if out.Samples == nil {
		out.Samples = map[string]json.RawMessage{}
	}
	return &out
}

// Export returns the projection for this template, or nil when none was built.
// A nil return means B2 has nothing safe to serve and must answer not-found
// rather than improvise a projection at request time.
func (m TemplateMeta) Export() *SafeExport { return m.export.Clone() }

// ExportHash is the deterministic digest, empty when there is no projection.
func (m TemplateMeta) ExportHash() string {
	if m.export == nil {
		return ""
	}
	return m.export.Hash
}

func sortedViewNames(views map[ViewKey]ViewSpec) []string {
	names := make([]string, 0, len(views))
	for view := range views {
		names = append(names, string(view))
	}
	sort.Strings(names)
	return names
}
