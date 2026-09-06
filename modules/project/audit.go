package project

import (
	"go.uber.org/zap"
)

// Audit actions. Low-cardinality enum: these values go into log fields and,
// eventually, into whatever queries those logs, so a typo must not become a new
// action silently.
const (
	auditCreate       = "project.create"
	auditUpdate       = "project.update"
	auditDisband      = "project.disband"
	auditMemberAdd    = "project.member_add"
	auditMemberRemove = "project.member_remove"
	auditLeave        = "project.leave"
	auditRoleChange   = "project.role_change"
	auditCascade      = "project.space_cascade"
)

// AuditEntry is one audit record.
//
// A struct plus an injectable sink rather than a bare log call, because otherwise the
// acceptance criterion "each write path emits an audit entry carrying the actor, the
// target and the reason" is not checkable: the only way to assert on a zap line routed
// through octo-lib's TLog is to capture the process's logger, which every parallel test
// would then fight over. The seam is the same trick middleware.go uses for the
// cache-DEL failure branch.
type AuditEntry struct {
	Action    string
	ActorUID  string
	TargetUID string
	ProjectID string
	SpaceID   string
	Reason    string
	Extra     []zap.Field
}

// auditSink receives every entry. Defaults to the zap emitter; tests swap it.
type auditSink func(AuditEntry)

// audit emits one structured audit record.
//
// A log line, not a table: this repo's only audit precedent is
// modules/messages_search/audit.go, which is structured zap output, and there is no audit
// table anywhere in the schema. If the product needs a QUERYABLE audit trail that is a new
// table and its own task — worth saying out loud, because a reader could otherwise assume
// this ships audit search.
//
// Fields are identity and low-cardinality enums only. No project names, no user names, no
// request bodies: the audit channel is shared with other ops use-cases that should not
// receive user content.
func (p *Project) audit(action, actorUID, targetUID, projectID, spaceID, reason string, extra ...zap.Field) {
	entry := AuditEntry{
		Action:    action,
		ActorUID:  actorUID,
		TargetUID: targetUID,
		ProjectID: projectID,
		SpaceID:   spaceID,
		Reason:    reason,
		Extra:     extra,
	}
	if p.auditSink != nil {
		p.auditSink(entry)
		return
	}
	p.emitAudit(entry)
}

// emitAudit is the default sink.
func (p *Project) emitAudit(e AuditEntry) {
	fields := make([]zap.Field, 0, 6+len(e.Extra))
	fields = append(fields,
		zap.String("audit", e.Action),
		zap.String("actor", e.ActorUID),
		zap.String("project_id", e.ProjectID),
		zap.String("space_id", e.SpaceID),
	)
	if e.TargetUID != "" {
		fields = append(fields, zap.String("target", e.TargetUID))
	}
	if e.Reason != "" {
		fields = append(fields, zap.String("reason", e.Reason))
	}
	fields = append(fields, e.Extra...)
	p.Info("project audit", fields...)
}
