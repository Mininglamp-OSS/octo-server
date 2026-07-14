package resourceshare

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	jose "github.com/go-jose/go-jose/v3"
	"github.com/gowebpki/jcs"
)

const (
	ProofField               = "resource_share_proof"
	proofVersion             = 1
	proofAlgorithm           = "ES256"
	proofDeliveryIDBytes     = 32
	maxProofVerificationKeys = 16
)

type ProofSigningKey struct {
	KeyID      string
	PrivateKey *ecdsa.PrivateKey
}

type ProofVerificationKey struct {
	KeyID     string
	PublicKey interface{}
}

type CanonicalProofTarget struct {
	Kind         TargetKind `json:"kind"`
	ParticipantA string     `json:"participant_a,omitempty"`
	ParticipantB string     `json:"participant_b,omitempty"`
	GroupNo      string     `json:"group_no,omitempty"`
	ShortID      string     `json:"short_id,omitempty"`
}

type ProofContext struct {
	ActorUID   string      `json:"actor_uid"`
	SpaceID    string      `json:"space_id"`
	ProviderID ProviderID  `json:"provider"`
	Resource   ResourceRef `json:"resource"`
	Target     Target      `json:"target"`
	DeliveryID string      `json:"delivery_id"`
}

type ProofObservation struct {
	ActorUID  string `json:"actor_uid"`
	ViewerUID string `json:"viewer_uid,omitempty"`
	SpaceID   string `json:"space_id"`
	Target    Target `json:"target"`
}

type ShareProof struct {
	Version    int                  `json:"v"`
	Algorithm  string               `json:"alg"`
	KeyID      string               `json:"kid"`
	ProviderID ProviderID           `json:"provider"`
	Resource   ResourceRef          `json:"resource"`
	ActorUID   string               `json:"actor_uid"`
	SpaceID    string               `json:"space_id"`
	Target     CanonicalProofTarget `json:"target"`
	DeliveryID string               `json:"delivery_id"`
	Signature  string               `json:"signature"`
}

type proofMetadata struct {
	Version    int                  `json:"v"`
	ProviderID ProviderID           `json:"provider"`
	Resource   ResourceRef          `json:"resource"`
	ActorUID   string               `json:"actor_uid"`
	SpaceID    string               `json:"space_id"`
	Target     CanonicalProofTarget `json:"target"`
	DeliveryID string               `json:"delivery_id"`
}

type proofSigningInput struct {
	Metadata proofMetadata          `json:"metadata"`
	Envelope map[string]interface{} `json:"envelope"`
}

func (p ShareProof) metadata() proofMetadata {
	return proofMetadata{
		Version:    p.Version,
		ProviderID: p.ProviderID,
		Resource:   p.Resource,
		ActorUID:   p.ActorUID,
		SpaceID:    p.SpaceID,
		Target:     p.Target,
		DeliveryID: p.DeliveryID,
	}
}

type ProofSigner struct {
	keyID  string
	signer jose.Signer
}

func NewProofSigner(input ProofSigningKey) (*ProofSigner, error) {
	if !keyIDPattern.MatchString(input.KeyID) {
		return nil, fmt.Errorf("%w: invalid active key id", ErrProofConfig)
	}
	privateKey, ok := copyP256PrivateKey(input.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: invalid P-256 private key", ErrProofConfig)
	}
	options := (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), input.KeyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: privateKey}, options)
	if err != nil {
		return nil, fmt.Errorf("%w: create signer: %v", ErrProofConfig, err)
	}
	return &ProofSigner{keyID: input.KeyID, signer: signer}, nil
}

func (s *ProofSigner) Seal(input map[string]interface{}, ctx ProofContext) (map[string]interface{}, error) {
	if s == nil || s.signer == nil {
		return nil, fmt.Errorf("%w: signer unavailable", ErrProofConfig)
	}
	payload, err := clonePayload(input)
	if err != nil {
		return nil, proofInvalid("snapshot payload", err)
	}
	if err := validateUnsignedShareEnvelope(payload); err != nil {
		return nil, err
	}
	metadata, err := metadataForSigning(ctx)
	if err != nil {
		return nil, err
	}

	payload["space_id"] = ctx.SpaceID
	if err := cardmsg.Validate(payload); err != nil {
		return nil, proofInvalid("card validation", err)
	}
	if err := cardmsg.Finalize(payload); err != nil {
		return nil, proofInvalid("card finalization", err)
	}
	canonical, err := canonicalProofInput(payload, metadata)
	if err != nil {
		return nil, proofInvalid("canonical signing input", err)
	}
	signed, err := s.signer.Sign(canonical)
	if err != nil {
		return nil, proofInvalid("sign", err)
	}
	detached, err := signed.DetachedCompactSerialize()
	if err != nil {
		return nil, proofInvalid("serialize signature", err)
	}
	proof := ShareProof{
		Version:    metadata.Version,
		Algorithm:  proofAlgorithm,
		KeyID:      s.keyID,
		ProviderID: metadata.ProviderID,
		Resource:   metadata.Resource,
		ActorUID:   metadata.ActorUID,
		SpaceID:    metadata.SpaceID,
		Target:     metadata.Target,
		DeliveryID: metadata.DeliveryID,
		Signature:  detached,
	}
	proofMap, err := structToMap(proof)
	if err != nil {
		return nil, proofInvalid("encode proof", err)
	}
	payload[ProofField] = proofMap
	if err := cardmsg.RecheckPayloadSize(payload); err != nil {
		return nil, proofInvalid("sealed payload size", err)
	}
	return payload, nil
}

type ProofVerifier struct {
	keys map[string]*ecdsa.PublicKey
	jwks jose.JSONWebKeySet
}

func NewProofVerifier(inputs []ProofVerificationKey) (*ProofVerifier, error) {
	if len(inputs) == 0 || len(inputs) > maxProofVerificationKeys {
		return nil, fmt.Errorf("%w: invalid verification key count", ErrProofConfig)
	}
	verifier := &ProofVerifier{keys: make(map[string]*ecdsa.PublicKey, len(inputs))}
	for _, input := range inputs {
		if !keyIDPattern.MatchString(input.KeyID) {
			return nil, fmt.Errorf("%w: invalid verification key id", ErrProofConfig)
		}
		if _, duplicate := verifier.keys[input.KeyID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate verification key id", ErrProofConfig)
		}
		publicKey, ok := copyP256PublicKey(input.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: invalid P-256 verification key", ErrProofConfig)
		}
		verifier.keys[input.KeyID] = publicKey
		verifier.jwks.Keys = append(verifier.jwks.Keys, jose.JSONWebKey{
			Key:       publicKey,
			KeyID:     input.KeyID,
			Algorithm: proofAlgorithm,
			Use:       "sig",
		})
	}
	sort.Slice(verifier.jwks.Keys, func(i, j int) bool { return verifier.jwks.Keys[i].KeyID < verifier.jwks.Keys[j].KeyID })
	return verifier, nil
}

func (v *ProofVerifier) JWKS() jose.JSONWebKeySet {
	if v == nil {
		return jose.JSONWebKeySet{}
	}
	keys := make([]jose.JSONWebKey, 0, len(v.jwks.Keys))
	for _, input := range v.jwks.Keys {
		publicKey, _ := copyP256PublicKey(input.Key)
		input.Key = publicKey
		keys = append(keys, input)
	}
	return jose.JSONWebKeySet{Keys: keys}
}

func (v *ProofVerifier) Verify(sealed map[string]interface{}, observation ProofObservation) error {
	if v == nil || len(v.keys) == 0 {
		return fmt.Errorf("%w: verifier unavailable", ErrProofInvalid)
	}
	proof, unsigned, err := splitSealedPayload(sealed)
	if err != nil {
		return err
	}
	if err := validateProofShape(proof); err != nil {
		return err
	}
	if err := validateUnsignedShareEnvelope(unsigned); err != nil {
		return err
	}
	if err := cardmsg.Validate(unsigned); err != nil {
		return proofInvalid("card validation", err)
	}
	card, ok := unsigned["card"].(map[string]interface{})
	if !ok || unsigned["plain"] != cardmsg.BuildPlain(card) {
		return proofInvalid("authoritative plain mismatch", nil)
	}
	spaceID, ok := unsigned["space_id"].(string)
	if !ok || spaceID == "" || spaceID != proof.SpaceID || spaceID != observation.SpaceID ||
		proof.ActorUID != observation.ActorUID {
		return proofInvalid("observed actor or space mismatch", nil)
	}
	observedTarget, err := proofTargetForObservation(observation)
	if err != nil || observedTarget != proof.Target {
		return proofInvalid("observed target mismatch", err)
	}

	canonical, err := canonicalProofInput(unsigned, proof.metadata())
	if err != nil {
		return proofInvalid("canonical signing input", err)
	}
	publicKey, ok := v.keys[proof.KeyID]
	if !ok {
		return proofInvalid("verification key unavailable", nil)
	}
	signed, err := jose.ParseDetached(proof.Signature, canonical)
	if err != nil || len(signed.Signatures) != 1 {
		return proofInvalid("malformed detached JWS", err)
	}
	signature := signed.Signatures[0]
	if !emptyJOSEHeader(signature.Unprotected) || signature.Protected.JSONWebKey != nil ||
		signature.Protected.Nonce != "" || len(signature.Protected.ExtraHeaders) != 0 ||
		signature.Protected.KeyID != proof.KeyID || signature.Protected.Algorithm != proof.Algorithm {
		return proofInvalid("untrusted JOSE header", nil)
	}
	if _, err := signed.Verify(publicKey); err != nil {
		return proofInvalid("signature verification", err)
	}
	return nil
}

func metadataForSigning(ctx ProofContext) (proofMetadata, error) {
	target, err := proofTargetForSend(ctx.ActorUID, ctx.Target)
	metadata := proofMetadata{
		Version:    proofVersion,
		ProviderID: ctx.ProviderID,
		Resource:   ctx.Resource,
		ActorUID:   ctx.ActorUID,
		SpaceID:    ctx.SpaceID,
		Target:     target,
		DeliveryID: ctx.DeliveryID,
	}
	if err != nil {
		return proofMetadata{}, proofInvalid("target", err)
	}
	if err := validateProofMetadata(metadata); err != nil {
		return proofMetadata{}, err
	}
	return metadata, nil
}

func validateProofShape(proof ShareProof) error {
	if proof.Algorithm != proofAlgorithm || !keyIDPattern.MatchString(proof.KeyID) ||
		strings.Count(proof.Signature, ".") != 2 || !strings.Contains(proof.Signature, "..") {
		return proofInvalid("proof algorithm, key, or signature invalid", nil)
	}
	return validateProofMetadata(proof.metadata())
}

func validateProofMetadata(metadata proofMetadata) error {
	if metadata.Version != proofVersion || !providerIDPattern.MatchString(string(metadata.ProviderID)) ||
		!validIdentifier(metadata.ActorUID, 1, maxActorUIDBytes) ||
		!validIdentifier(metadata.SpaceID, 1, maxSpaceIDBytes) ||
		!providerIDPattern.MatchString(metadata.Resource.Type) ||
		!validIdentifier(metadata.Resource.ID, 1, maxResourceIDBytes) ||
		!validIdentifier(metadata.Resource.Revision, 1, maxRevisionBytes) ||
		!validDeliveryID(metadata.DeliveryID) {
		return proofInvalid("metadata invalid", nil)
	}
	if err := validateCanonicalProofTarget(metadata.ActorUID, metadata.Target); err != nil {
		return proofInvalid("canonical target invalid", err)
	}
	return nil
}

func validDeliveryID(value string) bool {
	if len(value) != hex.EncodedLen(proofDeliveryIDBytes) || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == proofDeliveryIDBytes
}

func proofTargetForSend(actorUID string, target Target) (CanonicalProofTarget, error) {
	if _, err := canonicalTargetKey(actorUID, target); err != nil {
		return CanonicalProofTarget{}, err
	}
	switch target.Kind {
	case TargetDM:
		first, second := actorUID, target.PeerUID
		if second < first {
			first, second = second, first
		}
		return CanonicalProofTarget{Kind: TargetDM, ParticipantA: first, ParticipantB: second}, nil
	case TargetGroup:
		return CanonicalProofTarget{Kind: TargetGroup, GroupNo: target.GroupNo}, nil
	case TargetThread:
		return CanonicalProofTarget{Kind: TargetThread, GroupNo: target.GroupNo, ShortID: target.ShortID}, nil
	default:
		return CanonicalProofTarget{}, errors.New("unsupported target")
	}
}

func proofTargetForObservation(observation ProofObservation) (CanonicalProofTarget, error) {
	if !validIdentifier(observation.ActorUID, 1, maxActorUIDBytes) ||
		!validIdentifier(observation.SpaceID, 1, maxSpaceIDBytes) {
		return CanonicalProofTarget{}, errors.New("invalid observed actor or space")
	}
	if observation.Target.Kind == TargetDM {
		if !validIdentifier(observation.ViewerUID, 1, maxActorUIDBytes) ||
			observation.Target.GroupNo != "" || observation.Target.ShortID != "" ||
			!validIdentifier(observation.Target.PeerUID, 1, maxTargetIDBytes) ||
			observation.ViewerUID == observation.Target.PeerUID {
			return CanonicalProofTarget{}, errors.New("invalid observed dm")
		}
		if observation.ActorUID != observation.ViewerUID && observation.ActorUID != observation.Target.PeerUID {
			return CanonicalProofTarget{}, errors.New("actor is not a dm participant")
		}
		first, second := observation.ViewerUID, observation.Target.PeerUID
		if second < first {
			first, second = second, first
		}
		return CanonicalProofTarget{Kind: TargetDM, ParticipantA: first, ParticipantB: second}, nil
	}
	return proofTargetForSend(observation.ActorUID, observation.Target)
}

func validateCanonicalProofTarget(actorUID string, target CanonicalProofTarget) error {
	switch target.Kind {
	case TargetDM:
		if target.GroupNo != "" || target.ShortID != "" ||
			!validIdentifier(target.ParticipantA, 1, maxTargetIDBytes) ||
			!validIdentifier(target.ParticipantB, 1, maxTargetIDBytes) ||
			target.ParticipantA >= target.ParticipantB ||
			(actorUID != target.ParticipantA && actorUID != target.ParticipantB) {
			return errors.New("invalid canonical dm")
		}
	case TargetGroup:
		if target.ParticipantA != "" || target.ParticipantB != "" || target.ShortID != "" ||
			!validIdentifier(target.GroupNo, 1, maxTargetIDBytes) {
			return errors.New("invalid canonical group")
		}
	case TargetThread:
		if target.ParticipantA != "" || target.ParticipantB != "" ||
			!validIdentifier(target.GroupNo, 1, maxTargetIDBytes) ||
			!validIdentifier(target.ShortID, 1, maxTargetIDBytes) {
			return errors.New("invalid canonical thread")
		}
	default:
		return errors.New("unsupported canonical target")
	}
	return nil
}

func validateUnsignedShareEnvelope(payload map[string]interface{}) error {
	if len(payload) == 0 || !cardmsg.IsCardPayload(payload) || payload["profile"] != cardmsg.ProfileV1 {
		return proofInvalid("only octo/v1 card envelopes are supported", nil)
	}
	allowed := map[string]struct{}{
		"type": {}, "card_version": {}, "profile": {}, "card": {}, "plain": {}, "space_id": {},
	}
	for key := range payload {
		if _, ok := allowed[key]; !ok {
			return proofInvalid("unreviewed envelope field", nil)
		}
	}
	return nil
}

func splitSealedPayload(sealed map[string]interface{}) (ShareProof, map[string]interface{}, error) {
	if _, exists := sealed[ProofField]; !exists {
		return ShareProof{}, nil, ErrProofMissing
	}
	cloned, err := clonePayload(sealed)
	if err != nil {
		return ShareProof{}, nil, proofInvalid("snapshot sealed payload", err)
	}
	rawProof, exists := cloned[ProofField]
	if !exists {
		return ShareProof{}, nil, ErrProofMissing
	}
	proofBytes, err := json.Marshal(rawProof)
	if err != nil {
		return ShareProof{}, nil, proofInvalid("encode proof", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(proofBytes))
	decoder.DisallowUnknownFields()
	var proof ShareProof
	if err := decoder.Decode(&proof); err != nil {
		return ShareProof{}, nil, proofInvalid("decode proof", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ShareProof{}, nil, proofInvalid("multiple proof values", err)
	}
	delete(cloned, ProofField)
	return proof, cloned, nil
}

func canonicalProofInput(unsigned map[string]interface{}, metadata proofMetadata) ([]byte, error) {
	raw, err := json.Marshal(proofSigningInput{Metadata: metadata, Envelope: unsigned})
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

func clonePayload(input map[string]interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var cloned map[string]interface{}
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func structToMap(input interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var output map[string]interface{}
	if err := json.Unmarshal(raw, &output); err != nil {
		return nil, err
	}
	return output, nil
}

func copyP256PrivateKey(input *ecdsa.PrivateKey) (*ecdsa.PrivateKey, bool) {
	if input == nil || input.Curve != elliptic.P256() || input.D == nil || input.D.Sign() <= 0 ||
		input.D.Cmp(elliptic.P256().Params().N) >= 0 {
		return nil, false
	}
	x, y := elliptic.P256().ScalarBaseMult(input.D.Bytes())
	if input.X == nil || input.Y == nil || x.Cmp(input.X) != 0 || y.Cmp(input.Y) != 0 {
		return nil, false
	}
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).Set(x), Y: new(big.Int).Set(y)},
		D:         new(big.Int).Set(input.D),
	}, true
}

func proofInvalid(reason string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrProofInvalid, reason)
	}
	return fmt.Errorf("%w: %s: %v", ErrProofInvalid, reason, cause)
}
