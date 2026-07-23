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
	registry *Registry
	mutator  mutationGateway
}

func NewCardUpdater(registry *Registry, mutator mutationGateway) (CardUpdater, error) {
	if registry == nil || mutator == nil {
		return nil, errors.New("cardtmpl: updater dependencies are required")
	}
	return &updater{registry: registry, mutator: mutator}, nil
}

func (u *updater) ReplaceView(ctx context.Context, target UpdateTarget, id ID, version string,
	state State, fields json.RawMessage, env BuildEnv) (err error) {
	if err := validateUpdateTarget(ctx, target); err != nil {
		return err
	}
	if id == "" || version == "" || env.SpaceID != target.Target.SpaceID {
		return ErrUpdateInvalid
	}
	if _, err := u.registry.Lookup(id, version); err != nil {
		return err
	}
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		recordUpdate(id, version, result)
	}()
	document, profile, err := u.registry.RenderCard(ctx, id, version, state, fields, env)
	if err != nil {
		return err
	}
	contentEdit, err := updateEnvelope(document, profile, target.Target.SpaceID, target.CardSeq)
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
	if template, ok := cardmsg.CardTemplateContext(snapshot.Envelope); ok {
		if _, lookupErr := u.registry.Lookup(ID(template.ID), template.Version); lookupErr == nil {
			metricID, metricVersion = ID(template.ID), template.Version
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

func updateEnvelope(document json.RawMessage, profile, spaceID string, cardSeq int64) (string, error) {
	var card map[string]interface{}
	if err := json.Unmarshal(document, &card); err != nil || card == nil {
		return "", fmt.Errorf("%w: decode rendered card", ErrUpdateInvalid)
	}
	envelope := map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": profile, "space_id": spaceID, "card_seq": cardSeq, "card": card,
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
