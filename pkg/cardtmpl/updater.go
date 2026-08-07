package cardtmpl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

var ErrUpdateInvalid = errors.New("cardtmpl: invalid card update")

// UpdateTarget binds a render/update to one authoritative stored message.
// CardSeq is the positive sequence enforced by the CardMutator CAS. ReplaceView
// requires it strictly greater than the stored frame's card_seq (monotonic, as
// the CAS mandates). Append additionally requires it to be exactly consecutive
// (snapshot.CardSeq+1): a progress-frame appender owns its own frame numbering,
// so a gap signals a lost/out-of-order frame and is rejected as a conflict.
type UpdateTarget struct {
	Target     carddispatch.Target
	SenderUID  string
	MessageID  string
	MessageSeq uint32
	CardSeq    int64
}

type mutationGateway interface {
	Snapshot(context.Context, carddispatch.CardMutationTarget) (carddispatch.CardMutationSnapshot, error)
	Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error)
}

type CardUpdater interface {
	ReplaceView(context.Context, UpdateTarget, ID, string, State, json.RawMessage, BuildEnv) error
	Append(context.Context, UpdateTarget, json.RawMessage) error
}

type updater struct {
	catalog Catalog
	mutator mutationGateway
}

func NewCardUpdater(catalog Catalog, mutator mutationGateway) (CardUpdater, error) {
	if catalog == nil || mutator == nil {
		return nil, errors.New("cardtmpl: updater dependencies are required")
	}
	return &updater{catalog: catalog, mutator: mutator}, nil
}

func (u *updater) ReplaceView(ctx context.Context, target UpdateTarget, id ID, version string,
	state State, fields json.RawMessage, env BuildEnv) (err error) {
	if err := validateUpdateTarget(ctx, target); err != nil {
		return err
	}
	if id == "" || version == "" || env.SpaceID != target.Target.SpaceID {
		return ErrUpdateInvalid
	}
	// PR-C D3/G13：先 Snapshot 生效帧，再做任何 authorization/render。stored
	// server-authored 标记钉住 template identity / principal / Space —— 调用方
	// 的 UpdateTarget 与 id@version 只有与 stored 一致才被接受，不能靠它们把
	// 一张卡跨版本/跨身份/跨 Space 改写。
	snapshot, err := u.mutator.Snapshot(ctx, mutationTarget(target))
	if err != nil {
		return err
	}
	markers, err := storedMarkersForUpdate(snapshot.Envelope, target)
	if err != nil {
		return err
	}
	if markers.HasRef && (markers.Ref.ID != string(id) || markers.Ref.Version != version) {
		return fmt.Errorf("%w: stored template identity is %s@%s", ErrUpdateInvalid, markers.Ref.ID, markers.Ref.Version)
	}
	access, err := updaterCatalogAccess(target, markers)
	if err != nil {
		return err
	}
	meta, err := u.catalog.MetaExact(ctx, CatalogExactRequest{
		Access: access, ID: id, Version: version,
	})
	if err != nil {
		return err
	}
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		recordUpdate(meta.ID, meta.Version, result)
	}()
	document, profile, renderProfile, err := RenderCatalogCardWithProfiles(ctx, u.catalog, CatalogRenderRequest{
		Access: access, ID: id, Version: version,
		State: state, Fields: fields, Env: env,
	})
	if err != nil {
		return err
	}
	contentEdit, err := updateEnvelope(document, profile, renderProfile, target.Target.SpaceID, target.CardSeq, markers)
	if err != nil {
		return err
	}
	_, err = u.mutator.Mutate(ctx, mutationRequest(target, contentEdit))
	return err
}

func (u *updater) Append(ctx context.Context, target UpdateTarget, element json.RawMessage) (err error) {
	var metricID ID
	var metricVersion string
	defer func() {
		if metricID == "" {
			return
		}
		result := "ok"
		if err != nil {
			result = "error"
		}
		recordUpdate(metricID, metricVersion, result)
	}()
	if err := validateUpdateTarget(ctx, target); err != nil {
		return err
	}
	var appended map[string]interface{}
	if err := json.Unmarshal(element, &appended); err != nil || appended == nil {
		return ErrUpdateInvalid
	}
	snapshot, err := u.mutator.Snapshot(ctx, mutationTarget(target))
	if err != nil {
		return err
	}
	if snapshot.CardSeq < 0 || target.CardSeq-1 != snapshot.CardSeq {
		return carddispatch.ErrCardMutationConflict
	}
	// PR-C D3：stored 标记畸形即篡改信号 fail-close；provenance 存在时它是
	// edit principal 的唯一来源（re-marshal 会原样保留两个顶层标记）。
	markers, err := storedMarkersForUpdate(snapshot.Envelope, target)
	if err != nil {
		return err
	}
	access, err := updaterCatalogAccess(target, markers)
	if err != nil {
		return err
	}
	if template, ok := cardmsg.CardTemplateContext(snapshot.Envelope); ok {
		meta, lookupErr := u.catalog.MetaExact(ctx, CatalogExactRequest{
			Access: access, ID: ID(template.ID), Version: template.Version,
		})
		if lookupErr != nil && !errors.Is(lookupErr, ErrTemplateUnknown) {
			return lookupErr
		}
		if lookupErr == nil {
			metricID, metricVersion = meta.ID, meta.Version
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Envelope))
	decoder.UseNumber()
	var envelope map[string]interface{}
	if err := decoder.Decode(&envelope); err != nil || !cardmsg.IsCardPayload(envelope) {
		return ErrUpdateInvalid
	}
	spaceID, _ := envelope["space_id"].(string)
	if spaceID != target.Target.SpaceID {
		return ErrUpdateInvalid
	}
	card, _ := envelope["card"].(map[string]interface{})
	body, ok := card["body"].([]interface{})
	if !ok {
		return ErrUpdateInvalid
	}
	card["body"] = append(body, appended)
	envelope["card_seq"] = target.CardSeq
	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("%w: marshal append frame: %v", ErrUpdateInvalid, err)
	}
	normalized, err := cardmsg.NormalizeContentEdit(string(raw))
	if err != nil {
		return fmt.Errorf("%w: validate append frame: %v", ErrUpdateInvalid, err)
	}
	_, err = u.mutator.Mutate(ctx, mutationRequest(target, normalized))
	return err
}

// storedMarkersForUpdate 从生效帧提取 catalog 标记并做 update 边界校验：
// 畸形标记 fail-close；stored provenance 声明的 Space 非空时必须与权威
// UpdateTarget Space 一致 —— 跨 Space 的 mutation 在 render 前拒绝。
func storedMarkersForUpdate(envelope json.RawMessage, target UpdateTarget) (cardmsg.FrameCatalogMarkers, error) {
	markers, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil {
		return cardmsg.FrameCatalogMarkers{}, fmt.Errorf("%w: %v", ErrUpdateInvalid, err)
	}
	// Two different Spaces are in play here and the guard only concerns one of
	// them; an earlier version of this comment answered about the other, which
	// is why it read as if the empty case were already handled.
	//
	//   - target.Target.SpaceID — never empty. validateUpdateTarget rejects an
	//     empty one at the entry to both ReplaceView and Append, so by this
	//     point the caller has established a Space and a mismatch really is a
	//     mismatch. The neighbouring guards in bot_api and modules/message had
	//     to grow an explicit "unknown is not empty" state; this one does not,
	//     because the distinction is enforced one layer up and the click
	//     ingress refuses to create an event at all when the card's origin
	//     Space cannot be read.
	//   - markers.Provenance.SpaceID — *can* be empty; it is a documented
	//     reachable marker shape (pkg/cardmsg/provenance.go). The comparison
	//     below deliberately skips that case rather than treating "" as a
	//     mismatch against every target, because an unscoped marker is not a
	//     cross-Space edit. updaterCatalogAccess is what resolves it, by
	//     pinning the empty value to the target Space — see there.
	if markers.HasProvenance && markers.Provenance.SpaceID != "" &&
		markers.Provenance.SpaceID != target.Target.SpaceID {
		return cardmsg.FrameCatalogMarkers{}, fmt.Errorf("%w: stored provenance space %q does not match target space %q",
			ErrUpdateInvalid, markers.Provenance.SpaceID, target.Target.SpaceID)
	}
	return markers, nil
}

// updaterCatalogAccess 构造 historical_edit 的 catalog access。stored
// provenance 存在时它是 principal 的唯一来源（D3：不从 sender 反推）；
// legacy 无标记帧保留既有 sender 派生口径（static 历史兼容，invariant 7）。
//
// Review asked whether this fallback needs the same scoping the action ingress
// just grew — refuse a markerless frame that names a version only the runtime
// catalog knows. It does not, and the difference is the threat model rather
// than the shape. At the action ingress the template identity comes from
// `card.metadata.octo`, inside a card body a raw caller controls, and the
// derived principal is the *sending Bot's own* grant identity, so a fabricated
// frame turns an `edit` grant into a substitute for `send`. Here the identity
// is the caller's own `target.SenderUID`, every caller is an in-process
// internal producer, and `Snapshot` has already proven that sender owns the
// stored message. There is no path by which a request supplies it.
//
// Adding the check anyway would mean threading a Registry through
// NewCardUpdater purely to guard a hole nothing can reach, and it would refuse
// legitimate edits of pre-PR-C frames in any deployment whose default Registry
// is not the one the updater was built over. Recorded rather than done.
func updaterCatalogAccess(target UpdateTarget, markers cardmsg.FrameCatalogMarkers) (CatalogAccess, error) {
	if markers.HasProvenance {
		principal, err := CatalogPrincipalFromProvenance(markers.Provenance)
		if err != nil {
			return CatalogAccess{}, fmt.Errorf("%w: %v", ErrUpdateInvalid, err)
		}
		// Pin an empty provenance Space to the authoritative target Space,
		// exactly as the click ingress does (validatedFramePrincipal,
		// modules/message/api_card_action.go). An empty space_id is a
		// documented reachable marker shape, and storedMarkersForUpdate only
		// compares the *non-empty* case — so without this the two readers of
		// one marker derive two different principals from it.
		//
		// The difference is not cosmetic once grants exist: a principal with
		// SpaceID "" resolves against the global grant row alone, so an active
		// global grant plus an exact tombstone for the card's real Space would
		// be allowed, which is the shadowing invariant 11 exists to prevent.
		// Inert in this tree — nothing consumes Access on the static path — and
		// it refuses more rather than less, so it is safe to land ahead of the
		// grant machinery that makes it matter.
		if principal.SpaceID == "" {
			principal.SpaceID = target.Target.SpaceID
		}
		return CatalogAccess{Purpose: CatalogPurposeHistoricalEdit, Principal: principal}, nil
	}
	return CatalogAccess{
		Purpose: CatalogPurposeHistoricalEdit,
		Principal: CatalogPrincipal{
			Kind: CatalogPrincipalInternalProducer, ID: target.SenderUID, SpaceID: target.Target.SpaceID,
		},
	}, nil
}

func validateUpdateTarget(ctx context.Context, target UpdateTarget) error {
	if ctx == nil || target.SenderUID == "" || target.MessageID == "" || target.Target.SpaceID == "" ||
		target.Target.ChannelID == "" || target.Target.ChannelType == 0 || target.CardSeq <= 0 {
		return ErrUpdateInvalid
	}
	return nil
}

func mutationTarget(target UpdateTarget) carddispatch.CardMutationTarget {
	return carddispatch.CardMutationTarget{
		SenderUID: target.SenderUID, MessageID: target.MessageID, MessageSeq: target.MessageSeq,
		ChannelID: target.Target.ChannelID, ChannelType: target.Target.ChannelType,
	}
}

func mutationRequest(target UpdateTarget, contentEdit string) carddispatch.CardMutationRequest {
	return carddispatch.CardMutationRequest{
		SenderUID: target.SenderUID, MessageID: target.MessageID, MessageSeq: target.MessageSeq,
		ChannelID: target.Target.ChannelID, ChannelType: target.Target.ChannelType, ContentEdit: contentEdit,
	}
}

func updateEnvelope(document json.RawMessage, profile, renderProfile, spaceID string, cardSeq int64,
	markers cardmsg.FrameCatalogMarkers) (string, error) {
	var card map[string]interface{}
	if err := json.Unmarshal(document, &card); err != nil || card == nil {
		return "", fmt.Errorf("%w: decode rendered card", ErrUpdateInvalid)
	}
	envelope := map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": profile, "space_id": spaceID, "card_seq": cardSeq, "card": card,
	}
	if renderProfile != "" {
		envelope["render_profile"] = renderProfile
	}
	// PR-C D3：stored 标记原样保留到替换帧 —— identity/principal/Space 在
	// mutation 中不可变；legacy 无标记帧不凭空长出标记（principal 会是猜测）。
	if markers.HasRef {
		envelope[cardmsg.CatalogTemplateRefKey] = map[string]interface{}{
			"id": markers.Ref.ID, "version": markers.Ref.Version,
		}
	}
	if markers.HasProvenance {
		envelope[cardmsg.CatalogProvenanceKey] = markers.Provenance.MarshalMap()
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("%w: marshal replacement frame: %v", ErrUpdateInvalid, err)
	}
	normalized, err := cardmsg.NormalizeContentEdit(string(raw))
	if err != nil {
		return "", fmt.Errorf("%w: validate replacement frame: %v", ErrUpdateInvalid, err)
	}
	return normalized, nil
}
