package adminrbac

import (
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/authz"
	"go.uber.org/zap"
)

const (
	WorkplaceShadowEnv = "OCTO_ADMIN_RBAC_WORKPLACE_SHADOW"

	ShadowOutcomeMatch               = "match"
	ShadowOutcomeLegacyAllowRBACDeny = "legacy_allow_rbac_deny"
	ShadowOutcomeLegacyDenyRBACAllow = "legacy_deny_rbac_allow"
	ShadowOutcomeRBACEvaluationError = "rbac_evaluation_error"
	ShadowOutcomeMappingError        = "mapping_error"
	ShadowErrorMapping               = "mapping_error"
	ShadowErrorRBACEvaluation        = "rbac_evaluation_error"
)

// ShadowEvent is a non-persistent comparison between the existing legacy gate
// and the global RBAC result for one declared workplace operation.
type ShadowEvent struct {
	UID           string `json:"uid,omitempty"`
	OperationID   string `json:"operation_id"`
	Permission    string `json:"permission,omitempty"`
	LegacyAllowed bool   `json:"legacy_allowed"`
	RBACAllowed   bool   `json:"rbac_allowed"`
	Outcome       string `json:"outcome"`
	ErrorKind     string `json:"error_kind,omitempty"`
}

// ShadowSink is deliberately an in-process observation boundary. It does not
// persist authorization changes, access history, request bodies or tokens.
type ShadowSink interface {
	Observe(ShadowEvent)
}

type effectivePermissionProvider interface {
	EffectivePermissions(uid string) (EffectivePermissions, error)
}

// ShadowObserver evaluates only the permission mapped by a generated
// operation ID. It never accepts a resource selector, so workplace object
// identifiers cannot become Group, Space or Robot ACL scopes.
type ShadowObserver struct {
	provider effectivePermissionProvider
	enabled  func() bool
	sink     ShadowSink
}

func NewShadowObserver(provider effectivePermissionProvider, enabled func() bool, sink ShadowSink) *ShadowObserver {
	if enabled == nil {
		enabled = WorkplaceShadowEnabled
	}
	if sink == nil {
		sink = zapShadowSink{}
	}
	return &ShadowObserver{provider: provider, enabled: enabled, sink: sink}
}

// NewWorkplaceShadowObserver wires the observer to the existing evaluator and
// cache service. The feature switch remains off unless explicitly enabled.
func NewWorkplaceShadowObserver(ctx *config.Context) *ShadowObserver {
	service := NewService(NewStore(ctx.DB()), NewPermissionCache(ctx.Cache()))
	return NewShadowObserver(service, nil, nil)
}

func WorkplaceShadowEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(WorkplaceShadowEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (o *ShadowObserver) Observe(uid, operationID string, legacyAllowed bool) {
	if o == nil || o.enabled == nil || !o.enabled() {
		return
	}

	operation, ok := authz.LookupOperation(operationID)
	if !ok {
		if !strings.HasPrefix(operationID, "workplace.") {
			return
		}
		o.emit(ShadowEvent{
			UID:           uid,
			OperationID:   operationID,
			LegacyAllowed: legacyAllowed,
			Outcome:       ShadowOutcomeMappingError,
			ErrorKind:     ShadowErrorMapping,
		})
		return
	}
	if operation.Module != "workplace" || !strings.HasPrefix(operation.Path, "/v1/manager/workplace/") {
		return
	}
	if !authz.IsKnownPermission(operation.Permission) {
		o.emit(ShadowEvent{
			UID:           uid,
			OperationID:   operationID,
			Permission:    operation.Permission,
			LegacyAllowed: legacyAllowed,
			Outcome:       ShadowOutcomeMappingError,
			ErrorKind:     ShadowErrorMapping,
		})
		return
	}
	if o.provider == nil {
		o.emit(ShadowEvent{
			UID:           uid,
			OperationID:   operationID,
			Permission:    operation.Permission,
			LegacyAllowed: legacyAllowed,
			Outcome:       ShadowOutcomeRBACEvaluationError,
			ErrorKind:     ShadowErrorRBACEvaluation,
		})
		return
	}

	result, err := o.provider.EffectivePermissions(uid)
	if err != nil {
		o.emit(ShadowEvent{
			UID:           uid,
			OperationID:   operationID,
			Permission:    operation.Permission,
			LegacyAllowed: legacyAllowed,
			Outcome:       ShadowOutcomeRBACEvaluationError,
			ErrorKind:     ShadowErrorRBACEvaluation,
		})
		return
	}
	rbacAllowed, err := Allows(result, operation.Permission, "", "", "", "")
	if err != nil {
		o.emit(ShadowEvent{
			UID:           uid,
			OperationID:   operationID,
			Permission:    operation.Permission,
			LegacyAllowed: legacyAllowed,
			Outcome:       ShadowOutcomeRBACEvaluationError,
			ErrorKind:     ShadowErrorRBACEvaluation,
		})
		return
	}

	outcome := ShadowOutcomeMatch
	if legacyAllowed && !rbacAllowed {
		outcome = ShadowOutcomeLegacyAllowRBACDeny
	} else if !legacyAllowed && rbacAllowed {
		outcome = ShadowOutcomeLegacyDenyRBACAllow
	}
	o.emit(ShadowEvent{
		UID:           uid,
		OperationID:   operationID,
		Permission:    operation.Permission,
		LegacyAllowed: legacyAllowed,
		RBACAllowed:   rbacAllowed,
		Outcome:       outcome,
	})
}

func (o *ShadowObserver) emit(event ShadowEvent) {
	if o.sink != nil {
		o.sink.Observe(event)
	}
}

type zapShadowSink struct{}

func (zapShadowSink) Observe(event ShadowEvent) {
	fields := []zap.Field{
		zap.String("uid", event.UID),
		zap.String("operation_id", event.OperationID),
		zap.String("permission", event.Permission),
		zap.Bool("legacy_allowed", event.LegacyAllowed),
		zap.Bool("rbac_allowed", event.RBACAllowed),
		zap.String("outcome", event.Outcome),
	}
	if event.ErrorKind != "" {
		fields = append(fields, zap.String("error_kind", event.ErrorKind))
	}
	if event.Outcome == ShadowOutcomeMatch {
		zap.L().Debug("admin rbac workplace shadow", fields...)
		return
	}
	if event.ErrorKind != "" {
		zap.L().Error("admin rbac workplace shadow", fields...)
		return
	}
	zap.L().Warn("admin rbac workplace shadow mismatch", fields...)
}
