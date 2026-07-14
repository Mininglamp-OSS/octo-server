package resourceshare

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	jose "github.com/go-jose/go-jose/v3"
)

const (
	PlatformIntentVersion         = 1
	PlatformMaxCompactIntentBytes = 96 << 10
	PlatformMaxClaimsBytes        = 32 << 10
	PlatformMaxTargets            = 20
	PlatformMaxIntentLifetime     = 5 * time.Minute
	PlatformMaxClockSkew          = 30 * time.Second
)

type ProviderID string

type TargetKind string

const (
	TargetDM     TargetKind = "dm"
	TargetGroup  TargetKind = "group"
	TargetThread TargetKind = "thread"
)

type ResourceRef struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type TemplateRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type Target struct {
	Kind    TargetKind `json:"kind"`
	PeerUID string     `json:"peer_uid,omitempty"`
	GroupNo string     `json:"group_no,omitempty"`
	ShortID string     `json:"short_id,omitempty"`
}

type Intent struct {
	Version        int             `json:"v"`
	Provider       ProviderID      `json:"provider"`
	Issuer         string          `json:"iss"`
	Audience       string          `json:"aud"`
	ActorUID       string          `json:"actor_uid"`
	SpaceID        string          `json:"space_id"`
	IssuedAt       int64           `json:"iat"`
	ExpiresAt      int64           `json:"exp"`
	Nonce          string          `json:"nonce"`
	IdempotencyKey string          `json:"idempotency_key"`
	Resource       ResourceRef     `json:"resource"`
	Template       TemplateRef     `json:"template"`
	Targets        []Target        `json:"targets"`
	Claims         json.RawMessage `json:"claims"`
}

type IntentFingerprint [32]byte

type VerifiedIntent struct {
	ProviderID  ProviderID
	Intent      Intent
	Fingerprint IntentFingerprint
}

type ResourceCardField struct {
	Label string
	Value string
}

// ResourceCardInput is the bounded, presentation-neutral value returned by a
// reviewed provider adapter. It deliberately has no URL, card JSON, sender, or
// transport fields; the platform template and deep-link builder own those.
type ResourceCardInput struct {
	Title       string
	Description string
	Fields      []ResourceCardField
}

type ProviderAdapter interface {
	Revalidate(context.Context, VerifiedIntent) (*ResourceCardInput, error)
}

type VerificationKey struct {
	KeyID     string
	Algorithm jose.SignatureAlgorithm
	PublicKey interface{}
}

type ProviderLimits struct {
	MaxClaimsBytes    int
	MaxTargets        int
	MaxIntentLifetime time.Duration
	ClockSkew         time.Duration
}

type ClaimsValidator func(json.RawMessage) error

type DeepLinkBuilder func(ResourceRef) (*url.URL, error)

type ProviderSpec struct {
	ID               ProviderID
	Enabled          bool
	ResourceType     string
	Issuer           string
	Audience         string
	IntentVersion    int
	VerificationKeys []VerificationKey
	Templates        []TemplateRef
	Limits           ProviderLimits
	ValidateClaims   ClaimsValidator
	BuildDeepLink    DeepLinkBuilder
	Adapter          ProviderAdapter
}

type ReplayDisposition uint8

const (
	ReplayFirstUse ReplayDisposition = iota + 1
	ReplayRetry
)
