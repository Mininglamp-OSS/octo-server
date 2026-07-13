package carddispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/gocraft/dbr/v2"
)

type DBAuthorizer struct {
	session *dbr.Session
}

func NewDBAuthorizer(session *dbr.Session) *DBAuthorizer {
	return &DBAuthorizer{session: session}
}

func (a *DBAuthorizer) Authorize(ctx context.Context, identity *botidentity.Identity, target Target, policy AuthorizationPolicy) error {
	if ctx == nil || identity == nil || a == nil || a.session == nil {
		return denyTarget(errors.New("authorization dependencies unavailable"))
	}
	if err := ctx.Err(); err != nil {
		return denyTarget(err)
	}
	active, err := a.count("SELECT COUNT(*) FROM space WHERE space_id=? AND status=1", target.SpaceID)
	if err != nil {
		return denyTarget(fmt.Errorf("query target space: %w", err))
	}
	if active == 0 {
		return denyTarget(errors.New("target space is not active"))
	}

	switch target.ChannelType {
	case common.ChannelTypePerson.Uint8():
		return a.authorizeDM(identity, target, policy)
	case common.ChannelTypeGroup.Uint8():
		return a.authorizeGroup(identity, target.SpaceID, target.ChannelID, policy)
	case common.ChannelTypeCommunityTopic.Uint8():
		return a.authorizeThread(identity, target, policy)
	default:
		return denyTarget(errors.New("unsupported target type"))
	}
}

func (a *DBAuthorizer) authorizeDM(identity *botidentity.Identity, target Target, policy AuthorizationPolicy) error {
	targetMember, err := a.count(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		target.SpaceID, target.ChannelID,
	)
	if err != nil {
		return denyTarget(fmt.Errorf("query target membership: %w", err))
	}
	if targetMember == 0 {
		return denyTarget(errors.New("recipient is not an active member of target space"))
	}

	switch identity.Kind {
	case botidentity.KindAppBot:
		switch identity.AppScope {
		case botidentity.ScopePlatform:
		case botidentity.ScopeSpace:
			if identity.AppSpaceID == "" || identity.AppSpaceID != target.SpaceID {
				return denyTarget(errors.New("space app bot scope mismatch"))
			}
		default:
			return denyTarget(errors.New("unknown app bot scope"))
		}
		friend, err := a.friend(identity.UID, target.ChannelID)
		if err != nil {
			return denyTarget(fmt.Errorf("query app bot friend relation: %w", err))
		}
		if !friend {
			return denyTarget(errors.New("app bot conversation not started"))
		}
		return nil

	case botidentity.KindUserBot:
		if policy.SpacePolicy == SpacePolicySystemNotification {
			return nil
		}
		if policy.SpacePolicy != SpacePolicyMembership {
			return denyTarget(errors.New("unsupported user bot space policy"))
		}
		senderMember, err := a.count(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
			target.SpaceID, identity.UID,
		)
		if err != nil {
			return denyTarget(fmt.Errorf("query sender membership: %w", err))
		}
		if senderMember == 0 {
			return denyTarget(errors.New("sender is not authorized in target space"))
		}
		if identity.CreatorUID != "" && identity.CreatorUID == target.ChannelID {
			return nil
		}
		friend, err := a.friend(identity.UID, target.ChannelID)
		if err != nil {
			return denyTarget(fmt.Errorf("query user bot friend relation: %w", err))
		}
		if !friend {
			return denyTarget(errors.New("user bot target is not creator or friend"))
		}
		return nil
	default:
		return denyTarget(errors.New("identity is not a supported bot"))
	}
}

func (a *DBAuthorizer) authorizeGroup(identity *botidentity.Identity, spaceID, groupNo string, policy AuthorizationPolicy) error {
	if identity.Kind != botidentity.KindUserBot {
		return denyTarget(errors.New("app bots cannot target groups"))
	}
	var row struct {
		SpaceID string `db:"space_id"`
		Status  int    `db:"status"`
	}
	err := a.session.SelectBySql(
		"SELECT space_id, status FROM `group` WHERE group_no=? LIMIT 1", groupNo,
	).LoadOne(&row)
	if err != nil {
		return denyTarget(fmt.Errorf("query group: %w", err))
	}
	if row.Status != group.GroupStatusNormal || row.SpaceID == "" || row.SpaceID != spaceID {
		return denyTarget(errors.New("group is not active in target space"))
	}
	if policy.GroupPolicy == GroupPolicyMemberExempt {
		// Member-exempt posting does not require a membership row, but it must
		// still honor an explicit ban: a bot with a blacklisted membership row
		// was deliberately removed by a group admin and must not reappear via
		// this mode. Absence of any row (the pilot bot never joins) still posts.
		blacklisted, err := a.count(
			"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND status=? AND is_deleted=0",
			groupNo, identity.UID, int(common.GroupMemberStatusBlacklist),
		)
		if err != nil {
			return denyTarget(fmt.Errorf("query bot group blacklist: %w", err))
		}
		if blacklisted > 0 {
			return denyTarget(errors.New("bot is blacklisted from target group"))
		}
		return nil
	}
	if policy.GroupPolicy != GroupPolicyMemberRequired {
		return denyTarget(errors.New("unsupported group policy"))
	}
	member, err := a.count(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND status=1 AND is_deleted=0 AND is_external=0 AND robot=1",
		groupNo, identity.UID,
	)
	if err != nil {
		return denyTarget(fmt.Errorf("query bot group membership: %w", err))
	}
	if member == 0 {
		return denyTarget(errors.New("bot is not an active internal group member"))
	}
	return nil
}

func (a *DBAuthorizer) authorizeThread(identity *botidentity.Identity, target Target, policy AuthorizationPolicy) error {
	groupNo, shortID, err := thread.ParseChannelID(target.ChannelID)
	if err != nil {
		return denyTarget(err)
	}
	var status int
	err = a.session.SelectBySql(
		"SELECT status FROM thread WHERE group_no=? AND short_id=? LIMIT 1", groupNo, shortID,
	).LoadOne(&status)
	if err != nil {
		return denyTarget(fmt.Errorf("query thread: %w", err))
	}
	if status != thread.ThreadStatusActive {
		return denyTarget(errors.New("thread is not active"))
	}
	return a.authorizeGroup(identity, target.SpaceID, groupNo, policy)
}

func (a *DBAuthorizer) friend(uid, targetUID string) (bool, error) {
	count, err := a.count(
		"SELECT COUNT(*) FROM friend WHERE uid=? AND to_uid=? AND is_deleted=0",
		uid, targetUID,
	)
	return count > 0, err
}

func (a *DBAuthorizer) count(query string, args ...interface{}) (int, error) {
	var count int
	err := a.session.SelectBySql(query, args...).LoadOne(&count)
	return count, err
}

func denyTarget(cause error) error {
	return fmt.Errorf("%w: %v", ErrTargetDenied, cause)
}
