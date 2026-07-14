package resourceshare

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/gocraft/dbr/v2"
)

type HumanTargetAuthorizer struct {
	session *dbr.Session
	now     func() time.Time
}

func NewHumanTargetAuthorizer(session *dbr.Session) *HumanTargetAuthorizer {
	return &HumanTargetAuthorizer{session: session, now: time.Now}
}

func (a *HumanTargetAuthorizer) Authorize(ctx context.Context, actorUID, spaceID string, target Target) error {
	if ctx == nil {
		return targetQueryError("context unavailable", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || a.session == nil || a.now == nil {
		return targetQueryError("authorizer unavailable", errors.New("missing dependency"))
	}
	if !validIdentifier(actorUID, 1, maxActorUIDBytes) || !validIdentifier(spaceID, 1, maxSpaceIDBytes) {
		return targetDenied("actor or space invalid")
	}
	if _, err := canonicalTargetKey(actorUID, target); err != nil {
		return targetDenied("target invalid")
	}

	active, err := a.activeSpaceMember(ctx, spaceID, actorUID)
	if err != nil {
		return err
	}
	if !active {
		return targetDenied("actor is not active in target space")
	}

	switch target.Kind {
	case TargetDM:
		peerActive, err := a.activeSpaceMember(ctx, spaceID, target.PeerUID)
		if err != nil {
			return err
		}
		if !peerActive {
			return targetDenied("peer is not active in target space")
		}
		return nil
	case TargetGroup:
		return a.authorizeGroup(ctx, actorUID, spaceID, target.GroupNo)
	case TargetThread:
		if err := a.authorizeGroup(ctx, actorUID, spaceID, target.GroupNo); err != nil {
			return err
		}
		return a.authorizeThread(ctx, target.GroupNo, target.ShortID)
	default:
		return targetDenied("target kind unsupported")
	}
}

func (a *HumanTargetAuthorizer) activeSpaceMember(ctx context.Context, spaceID, uid string) (bool, error) {
	var count int
	err := a.session.SelectBySql(
		"SELECT COUNT(*) FROM space s JOIN space_member sm ON sm.space_id=s.space_id "+
			"WHERE s.space_id=? AND s.status=1 AND sm.uid=? AND sm.status=1",
		spaceID, uid,
	).LoadOneContext(ctx, &count)
	if err != nil {
		return false, targetQueryError("query active space membership", err)
	}
	return count == 1, nil
}

type humanGroupAccessRow struct {
	SpaceID            string `db:"space_id"`
	GroupStatus        int    `db:"group_status"`
	Forbidden          int    `db:"forbidden"`
	MemberStatus       int    `db:"member_status"`
	IsDeleted          int    `db:"is_deleted"`
	Role               int    `db:"role"`
	ForbiddenExpirTime int64  `db:"forbidden_expir_time"`
}

func (a *HumanTargetAuthorizer) authorizeGroup(ctx context.Context, actorUID, spaceID, groupNo string) error {
	var row humanGroupAccessRow
	err := a.session.SelectBySql(
		"SELECT g.space_id, g.status AS group_status, g.forbidden, "+
			"gm.status AS member_status, gm.is_deleted, gm.role, gm.forbidden_expir_time "+
			"FROM `group` g JOIN group_member gm ON gm.group_no=g.group_no "+
			"WHERE g.group_no=? AND gm.uid=? LIMIT 1",
		groupNo, actorUID,
	).LoadOneContext(ctx, &row)
	if errors.Is(err, dbr.ErrNotFound) {
		return targetDenied("group or membership unavailable")
	}
	if err != nil {
		return targetQueryError("query group access", err)
	}
	if row.SpaceID == "" || row.SpaceID != spaceID || row.GroupStatus != group.GroupStatusNormal {
		return targetDenied("group is not active in exact space")
	}
	if row.MemberStatus != int(common.GroupMemberStatusNormal) || row.IsDeleted != 0 {
		return targetDenied("group member is not active")
	}
	if row.ForbiddenExpirTime > a.now().Unix() {
		return targetDenied("group member is muted")
	}
	if row.Forbidden == 1 && row.Role != int(common.GroupMemberRoleCreater) && row.Role != int(common.GroupMemberRoleManager) {
		return targetDenied("group posting is restricted")
	}
	return nil
}

func (a *HumanTargetAuthorizer) authorizeThread(ctx context.Context, groupNo, shortID string) error {
	var status int
	err := a.session.SelectBySql(
		"SELECT status FROM thread WHERE group_no=? AND short_id=? LIMIT 1",
		groupNo, shortID,
	).LoadOneContext(ctx, &status)
	if errors.Is(err, dbr.ErrNotFound) {
		return targetDenied("thread unavailable")
	}
	if err != nil {
		return targetQueryError("query thread lifecycle", err)
	}
	if status != thread.ThreadStatusActive {
		return targetDenied("thread is not active")
	}
	return nil
}

func targetDenied(reason string) error {
	return fmt.Errorf("%w: %s", ErrTargetDenied, reason)
}

func targetQueryError(operation string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrTargetQuery, operation, cause)
}
