package group

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/gocraft/dbr/v2"
)

// ErrBotOwnershipDenied is returned when the inviter is not the creator of a
// bot they are trying to add to a group.
//
// Security context: YUJ-46 (github.com/Mininglamp-OSS/octo-server issue #1181).
// Before this check, any group member could add any bot to any group,
// bypassing the bot creator's consent. The strict default enforced here is
// that **only a bot's creator can invite their bot into any group**; group
// admins/owners and arbitrary members cannot invite other people's bots.
//
// Product rules currently deferred (see PR / issue for decision trail):
//   - Group admins do NOT get a pass for cross-creator bot invites.
//   - No system-bot / public-bot whitelist exists yet.
//   - Whether a bot is auto-kicked when its creator leaves is out of scope.
var ErrBotOwnershipDenied = errors.New("no permission to invite this bot")

// ErrAvatarOrdinaryGroupDenied keeps organization-managed avatars out of every
// user-managed group. Their only group membership is the server-owned AI Team
// projection, admitted through the narrow helpers in admission.go.
var ErrAvatarOrdinaryGroupDenied = errors.New("digital avatars can only join AI Team groups")

// checkBotOwnership verifies that every bot UID in memberUIDs was created by
// inviterUID. Non-bot UIDs are ignored. An empty inviterUID or empty
// memberUIDs slice results in nil (nothing to check).
//
// Rules:
//   - user.robot=1 AND robot.status=1 AND robot.creator_uid == inviterUID → OK
//   - user.robot=1 AND (robot row missing / status!=1 / creator_uid empty or
//     mismatched) → ErrBotOwnershipDenied
//   - user.robot=0 (human) → always OK (outside this function's scope)
//
// A single batch SQL query is used to avoid N+1. Only low-level SQL errors
// bubble up as raw errors; policy rejections always surface as
// ErrBotOwnershipDenied so callers can map to HTTP 403.
func checkBotOwnership(session dbr.SessionRunner, inviterUID string, memberUIDs []string) error {
	if inviterUID == "" || len(memberUIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(memberUIDs))
	uniq := make([]string, 0, len(memberUIDs))
	for _, uid := range memberUIDs {
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		uniq = append(uniq, uid)
	}
	if len(uniq) == 0 {
		return nil
	}
	type botRow struct {
		UID          string         `db:"uid"`
		CreatorUID   string         `db:"creator_uid"`
		Kind         botpolicy.Kind `db:"kind"`
		AvatarActive bool           `db:"avatar_active"`
	}
	var rows []botRow
	// LEFT JOIN on robot.status=1 matches the semantics used elsewhere
	// (modules/user/db_pinned.go QueryPeerRobotInfo, modules/robot/event.go
	// existRobot): a user flagged as a bot but whose robot row is missing
	// or inactive is treated as having no valid owner, and therefore
	// cannot be invited by anyone (fail-closed).
	_, err := session.SelectBySql(
		"SELECT u.uid AS uid, IFNULL(r.creator_uid,'') AS creator_uid, IFNULL(r.kind,'') AS kind, "+
			botpolicy.ActiveAvatarSQL("r")+" AS avatar_active "+
			"FROM `user` u LEFT JOIN robot r ON r.robot_id = u.uid AND r.status = 1 "+
			"WHERE u.robot = 1 AND u.uid IN ?",
		uniq,
	).Load(&rows)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Kind == botpolicy.Avatar && r.AvatarActive {
			continue
		}
		if r.CreatorUID == "" || r.CreatorUID != inviterUID {
			return ErrBotOwnershipDenied
		}
	}
	return nil
}

// checkAvatarOrdinaryGroupAdmission rejects Avatar membership before an
// ordinary group invite is persisted. AI Team never uses this entry point: its
// private and aggregate projections use the transaction-scoped, purpose-checked
// admission bridge instead. This makes normal-group APIs and service callers
// fail closed by the same rule.
func checkAvatarOrdinaryGroupAdmission(session dbr.SessionRunner, memberUIDs []string) error {
	if len(memberUIDs) == 0 {
		return nil
	}
	var count int
	err := session.SelectBySql("SELECT COUNT(*) FROM robot WHERE kind='avatar' AND robot_id IN ?", memberUIDs).LoadOne(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return ErrAvatarOrdinaryGroupDenied
	}
	return nil
}
