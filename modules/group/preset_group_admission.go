package group

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"go.uber.org/zap"
)

// registerPresetGroupAdmitter wires modules/space's preset-group auto-join to
// this module's single admission entry.
//
// Reverse-registered for the same reason the Space member-removal cleanup step
// is: modules/group already imports modules/space, so the other direction would
// be an import cycle. modules/space previously worked around that with raw DML
// (`INSERT INTO group_member (group_no, uid) VALUES (?, ?)`), which is the write
// the source guard flags and the four defects preset_group_admitter.go lists.
//
// Called from 1module.go at module construction, beside
// registerSpaceMemberRemovalCleanup.
func (g *Group) registerPresetGroupAdmitter() {
	spacemod.RegisterPresetGroupAdmitter(g.admitToPresetGroup)
}

// admitToPresetGroup adds uid to one of a Space's preset groups.
//
// It is called from a goroutine that modules/space launches after a Space join
// commits, once per preset group, and its failures are logged and skipped there.
// So it must be self-contained: its own transaction, its own version, and its
// own IM subscribe.
//
// # What this now writes that the raw INSERT did not
//
// version (incremental member sync sees the row), vercode, role, invite_uid,
// is_external / source_space_id, and the IM subscription. And because admission
// is an upsert keyed on the unique index, a uid who previously LEFT the group is
// restored rather than colliding with 1062 — the defect that made leaving a
// preset group permanent.
//
// # Why invite_uid is the joiner themself
//
// Nobody invited them; the Space's configuration did. Attributing it to an
// operator would put a name on an action that person did not take, and the
// alternatives (empty, or a system uid) are worse: empty loses the fact that
// this was self-service, and a system uid claims a bot did it.
func (g *Group) admitToPresetGroup(ctx *config.Context, spaceID, groupNo, uid string) error {
	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("group: preset admission GenSeq: %w", err)
	}

	tx, err := ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("group: preset admission begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// projectID is deliberately passed as the empty sentinel rather than read
	// from the group row: modules/space has ALREADY refused to auto-join a group
	// whose project_id is non-empty, and a preset group is never a project group
	// by construction. Passing "" here would be a fail-OPEN shortcut if that
	// check were ever removed, so the gate is kept honest by reading the row.
	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		return fmt.Errorf("group: preset admission query group: %w", err)
	}
	if groupModel == nil {
		return fmt.Errorf("group: preset admission: group %s not found", groupNo)
	}

	if err := g.db.admitOrRestoreMembersTx(tx, groupNo, groupModel.SpaceID, groupModel.ProjectID,
		[]MemberAdmission{{
			UID:       uid,
			Version:   version,
			Role:      MemberRoleCommon,
			InviteUID: uid,
		}}, AdmissionEntryPresetGroups); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: preset admission commit: %w", err)
	}

	// IM subscribe AFTER the commit, as every other admission path does: it is a
	// blocking HTTP call to WuKongIM and must not be held inside a transaction.
	//
	// A failure here leaves the member in the group but not on the channel, so
	// they see it and receive nothing. That is the same exposure every other
	// admission path carries; it is not re-solved here (see the brief's D6 — the
	// IM outbox is separately tracked in #797). Reported as an error so the
	// caller logs it rather than claiming success.
	if err := ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{uid},
	}); err != nil {
		g.Error("预设群组 IM 订阅失败，成员已入库但收不到消息",
			zap.String("group_no", groupNo), zap.String("uid", uid), zap.Error(err))
		return fmt.Errorf("group: preset admission IM subscribe: %w", err)
	}
	return nil
}
