package carddispatch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"go.uber.org/zap"
)

const maxProducerIDBytes = 64

var producerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type Registry struct {
	senders map[ProducerID]*producerSender
	invalid map[ProducerID]struct{}
	// bindings 是 ProducerID → 权威 SenderUID 的只读事实表（PR-C D3）。
	// action-context 用它校验 stored provenance：帧声明的 internal producer
	// 是否真的绑定到该消息的 FromUID。disabled 的 spec 也登记 —— 停用只收回
	// 发送能力，不抹掉身份事实（历史帧仍需校验）；duplicate/畸形 spec 身份
	// 有歧义，不登记。
	bindings map[ProducerID]string
}

func NewRegistry(deps Dependencies, specs []ProducerSpec) *Registry {
	registry := &Registry{
		senders:  make(map[ProducerID]*producerSender),
		invalid:  make(map[ProducerID]struct{}),
		bindings: make(map[ProducerID]string),
	}
	counts := make(map[ProducerID]int, len(specs))
	for _, spec := range specs {
		counts[spec.ID]++
	}
	duplicateRejected := make(map[ProducerID]struct{})
	for _, input := range specs {
		if counts[input.ID] > 1 {
			if _, rejected := duplicateRejected[input.ID]; !rejected {
				registry.reject(deps, input.ID, "duplicate")
				duplicateRejected[input.ID] = struct{}{}
			}
			continue
		}
		registry.recordBinding(input)
		if !input.Enabled {
			registry.invalid[input.ID] = struct{}{}
			continue
		}
		spec := copyProducerSpec(input)
		if reason := validateSpec(spec, deps); reason != "" {
			registry.reject(deps, spec.ID, reason)
			continue
		}
		featureEnabled := deps.FeatureEnabled
		if featureEnabled == nil {
			featureEnabled = cardmsg.Enabled
		}
		registry.senders[spec.ID] = &producerSender{
			spec:             spec,
			identityResolver: deps.IdentityResolver,
			authorizer:       deps.Authorizer,
			transport:        deps.Transport,
			metrics:          deps.Metrics,
			logger:           deps.Logger,
			featureEnabled:   featureEnabled,
			slots:            make(chan struct{}, spec.MaxInFlight),
		}
	}
	return registry
}

func (r *Registry) Sender(id ProducerID) (Sender, error) {
	if r == nil {
		return nil, categorized(CategoryProducerDisabled, errors.New("registry unavailable"))
	}
	sender, ok := r.senders[id]
	if !ok {
		return nil, categorized(CategoryProducerDisabled, fmt.Errorf("producer %q unavailable", id))
	}
	return sender, nil
}

// recordBinding 登记一条身份事实。只接受形状合法的 (ID, SenderUID)：ID 走与
// 发送侧相同的 pattern，sender 非空且非 incoming-webhook 前缀。
func (r *Registry) recordBinding(spec ProducerSpec) {
	if !producerIDPattern.MatchString(string(spec.ID)) {
		return
	}
	sender := strings.TrimSpace(spec.SenderUID)
	if sender == "" || strings.HasPrefix(sender, "iwh_") {
		return
	}
	r.bindings[spec.ID] = sender
}

// ProducerBinding 返回 ProducerID 声明的权威 SenderUID。只读事实，不携带
// 发送能力；调用方不能据此拿到 Sender。
func (r *Registry) ProducerBinding(id ProducerID) (string, bool) {
	if r == nil {
		return "", false
	}
	sender, ok := r.bindings[id]
	return sender, ok
}

func (r *Registry) reject(deps Dependencies, id ProducerID, reason string) {
	r.invalid[id] = struct{}{}
	delete(r.senders, id)
	delete(r.bindings, id)
	producer := metricProducerLabel(id)
	deps.Metrics.configError(producer, reason)
	if deps.Logger != nil {
		deps.Logger.Error("card dispatch producer configuration rejected",
			zap.String("producer", producer), zap.String("reason", reason))
	}
}

func copyProducerSpec(input ProducerSpec) ProducerSpec {
	input.AllowedChannelTypes = append([]uint8(nil), input.AllowedChannelTypes...)
	input.AllowedProfiles = append([]string(nil), input.AllowedProfiles...)
	return input
}

func validateSpec(spec ProducerSpec, deps Dependencies) string {
	if len(spec.ID) == 0 || len(spec.ID) > maxProducerIDBytes || !producerIDPattern.MatchString(string(spec.ID)) {
		return "invalid_id"
	}
	if strings.TrimSpace(spec.SenderUID) == "" || strings.HasPrefix(spec.SenderUID, "iwh_") {
		return "invalid_sender"
	}
	if spec.MaxInFlight <= 0 || spec.MaxInFlight > 1000 {
		return "invalid_concurrency"
	}
	if len(spec.AllowedChannelTypes) == 0 || !validChannelTypes(spec.AllowedChannelTypes) {
		return "invalid_targets"
	}
	if len(spec.AllowedProfiles) == 0 || !validProfiles(spec.AllowedProfiles, spec.ActionEventOwner) {
		return "invalid_profiles"
	}
	if spec.SpacePolicy != SpacePolicyMembership && spec.SpacePolicy != SpacePolicySystemNotification {
		return "invalid_space_policy"
	}
	if spec.GroupPolicy != GroupPolicyMemberRequired && spec.GroupPolicy != GroupPolicyMemberExempt {
		return "invalid_group_policy"
	}
	if deps.IdentityResolver == nil || deps.Authorizer == nil || deps.Transport == nil {
		return "missing_dependency"
	}
	return ""
}

func validChannelTypes(types []uint8) bool {
	seen := make(map[uint8]struct{}, len(types))
	for _, channelType := range types {
		switch channelType {
		case common.ChannelTypePerson.Uint8(), common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
		default:
			return false
		}
		if _, duplicate := seen[channelType]; duplicate {
			return false
		}
		seen[channelType] = struct{}{}
	}
	return true
}

func validProfiles(profiles []string, actionEventOwner string) bool {
	accepted := make(map[string]struct{})
	for _, profile := range cardmsg.AcceptedProfiles() {
		accepted[profile] = struct{}{}
	}
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if _, ok := accepted[profile]; !ok {
			return false
		}
		if _, duplicate := seen[profile]; duplicate {
			return false
		}
		seen[profile] = struct{}{}
		if profile == cardmsg.ProfileV2 {
			owner := strings.TrimSpace(actionEventOwner)
			if len(owner) == 0 || len(owner) > maxProducerIDBytes || !producerIDPattern.MatchString(owner) {
				return false
			}
		}
	}
	return true
}

func metricProducerLabel(id ProducerID) string {
	if producerIDPattern.MatchString(string(id)) {
		return string(id)
	}
	return "invalid"
}
