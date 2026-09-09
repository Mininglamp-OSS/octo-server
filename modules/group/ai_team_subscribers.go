package group

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/gocraft/dbr/v2"
)

const aiTeamProjectionPending = 1

// markAITeamLifecycleRosterRemovalTx makes an authoritative account/seat
// teardown visible to the aggregate projection in the same transaction as the
// managed-group membership removal. Deactivating the Agent here is important:
// Bot/Space status changes may commit only after group cleanup returns, while a
// concurrent stale projection must already observe that this identity is no
// longer desired.
func markAITeamLifecycleRosterRemovalTx(tx *dbr.Tx, model *Model, removedUIDs []string) error {
	if tx == nil || model == nil || len(removedUIDs) == 0 {
		return nil
	}
	type ownerKey struct {
		SpaceID string `db:"space_id"`
		UserUID string `db:"user_uid"`
	}
	var owners []*ownerKey
	switch model.Purpose {
	case aiteampkg.TeamGroupPurpose:
		owners = append(owners, &ownerKey{SpaceID: model.SpaceID, UserUID: model.Creator})
	case aiteampkg.GroupPurpose:
		if _, err := tx.Select("DISTINCT space_id", "user_uid").From("ai_team_agent").
			Where("group_no=? AND bot_id IN ?", model.GroupNo, uniqueSortedUIDs(removedUIDs)).Load(&owners); err != nil {
			return fmt.Errorf("query affected AI-team owners: %w", err)
		}
	default:
		return nil
	}
	for _, owner := range owners {
		if _, err := tx.UpdateBySql(`UPDATE ai_team_group
			SET state=?,last_error='',roster_version=roster_version+1,retry_after=CURRENT_TIMESTAMP
			WHERE space_id=? AND user_uid=?`, aiTeamProjectionPending, owner.SpaceID, owner.UserUID).Exec(); err != nil {
			return fmt.Errorf("advance AI-team roster version after lifecycle removal: %w", err)
		}
		if _, err := tx.Update("ai_team_agent").Set("is_added", 0).
			Where("space_id=? AND user_uid=? AND bot_id IN ?", owner.SpaceID, owner.UserUID, removedUIDs).Exec(); err != nil {
			return fmt.Errorf("deactivate lifecycle-removed AI-team agents: %w", err)
		}
	}
	return nil
}

// SyncAITeamGroupSubscribers applies the authoritative AI-team roster to the
// parent channel and every non-deleted subarea. WuKongIM's channel upsert is
// additive, so excluded identities must be removed explicitly; otherwise a
// deleted Agent keeps receiving messages and a late-added Agent cannot post in
// existing subareas.
//
// The caller passes a versioned roster and subarea snapshot prepared in the
// preceding short DB transaction. Every operation is idempotent; after this
// returns, the caller uses the roster version to detect a stale projection and
// replay the current snapshot without holding DB resources across this I/O.
func SyncAITeamGroupSubscribers(
	ctx *config.Context,
	groupNo, spaceID string,
	shortIDs, desired, excluded []string,
) error {
	shortIDs = uniqueSortedUIDs(shortIDs)
	desired = uniqueSortedUIDs(desired)
	excluded = uniqueSortedUIDs(excluded)

	var errs []error
	if err := ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: desired,
	}); err != nil {
		errs = append(errs, fmt.Errorf("upsert parent subscribers: %w", err))
	}
	if len(excluded) > 0 {
		if err := ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: excluded,
		}); err != nil {
			errs = append(errs, fmt.Errorf("remove parent subscribers: %w", err))
		}
	}

	for _, shortID := range shortIDs {
		channelID := groupNo + "____" + shortID
		if len(desired) > 0 {
			if addErr := ctx.IMAddSubscriber(&config.SubscriberAddReq{
				ChannelID: channelID, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Subscribers: desired,
			}); addErr != nil {
				errs = append(errs, fmt.Errorf("add subarea %s subscribers: %w", shortID, addErr))
			}
		}
		if len(excluded) > 0 {
			if removeErr := ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
				ChannelID: channelID, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Subscribers: excluded,
			}); removeErr != nil {
				errs = append(errs, fmt.Errorf("remove subarea %s subscribers: %w", shortID, removeErr))
			}
		}
	}

	if len(excluded) > 0 {
		for _, uid := range excluded {
			for _, shortID := range shortIDs {
				channelID := groupNo + "____" + shortID
				user.RemovePinnedForUserInSpace(uid, spaceID, channelID, common.ChannelTypeCommunityTopic.Uint8())
				conversation_ext.RemoveConvExtForUserInSpace(uid, spaceID, channelID, common.ChannelTypeCommunityTopic.Uint8())
			}
		}
	}
	return errors.Join(errs...)
}

// PrepareAITeamGroupSubscriberProjectionTx records the active/archived subarea
// snapshot and removes excluded users' thread-local state in the caller's short
// transaction. No external I/O is performed here. Keeping these deletes in the
// same transaction as the authoritative roster snapshot prevents a rollback
// from leaving partially-applied cleanup on another DB connection.
func PrepareAITeamGroupSubscriberProjectionTx(
	tx *dbr.Tx,
	groupNo string,
	excluded []string,
) ([]string, error) {
	excluded = uniqueSortedUIDs(excluded)
	var shortIDs []string
	if _, err := tx.Select("short_id").From("thread").
		Where("group_no=? AND status!=3", groupNo).OrderAsc("short_id").Load(&shortIDs); err != nil {
		return nil, fmt.Errorf("query AI-team subareas: %w", err)
	}
	if len(excluded) == 0 {
		return shortIDs, nil
	}
	if _, err := tx.DeleteFrom("thread_member").
		Where("uid IN ? AND thread_id IN (SELECT id FROM thread WHERE group_no=?)", excluded, groupNo).Exec(); err != nil {
		return nil, fmt.Errorf("delete excluded AI-team thread members: %w", err)
	}
	if _, err := tx.DeleteFrom("thread_setting").
		Where("group_no=? AND uid IN ?", groupNo, excluded).Exec(); err != nil {
		return nil, fmt.Errorf("delete excluded AI-team thread settings: %w", err)
	}
	return shortIDs, nil
}

func uniqueSortedUIDs(uids []string) []string {
	seen := make(map[string]struct{}, len(uids))
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}
