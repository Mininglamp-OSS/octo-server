// Package carddispatch is the sole supported boundary for trusted in-process
// InteractiveCard origination. It owns producer capability, authorization,
// envelope construction, final bytes, concurrency, and transport semantics.
package carddispatch

import (
	"context"
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/config"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
)

type ProducerID string

type SpacePolicy string

const (
	SpacePolicyMembership         SpacePolicy = "membership"
	SpacePolicySystemNotification SpacePolicy = "system_notification"
)

type GroupPolicy string

const (
	GroupPolicyMemberRequired GroupPolicy = "member_required"
	GroupPolicyMemberExempt   GroupPolicy = "member_exempt"
)

type Target struct {
	SpaceID     string
	ChannelID   string
	ChannelType uint8
}

type Card struct {
	Profile  string
	Document json.RawMessage
}

type Result struct {
	MessageID   int64
	MessageSeq  uint32
	ClientMsgNo string
}

type Sender interface {
	Send(ctx context.Context, target Target, card Card) (*Result, error)
}

type ProducerSpec struct {
	ID                  ProducerID
	Enabled             bool
	SenderUID           string
	AllowedChannelTypes []uint8
	AllowedProfiles     []string
	SpacePolicy         SpacePolicy
	GroupPolicy         GroupPolicy
	ActionEventOwner    string
	MaxInFlight         int
}

type AuthorizationPolicy struct {
	SpacePolicy SpacePolicy
	GroupPolicy GroupPolicy
}

type IdentityResolver interface {
	Resolve(uid string) (*botidentity.Identity, error)
}

type Authorizer interface {
	Authorize(ctx context.Context, identity *botidentity.Identity, target Target, policy AuthorizationPolicy) error
}

type Transport interface {
	SendMessageWithResult(req *config.MsgSendReq) (*config.MsgSendResp, error)
}

type Dependencies struct {
	IdentityResolver IdentityResolver
	Authorizer       Authorizer
	Transport        Transport
	Metrics          *Metrics
	Logger           liblog.Log
	FeatureEnabled   func() bool
}
