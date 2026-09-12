package group

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/gocraft/dbr/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Group-membership admission is the single native membership write primitive.
//
// Every path that adds or restores a group_member row uses
// admitOrRestoreMembersTx.  Project affiliation is deliberately not consulted
// here: native group membership and Project membership are independent sources
// of access.  Relation endpoints and Project-backed group creation perform
// their own Project authorization where the operation requires it.
const admissionMetricNamespace = "group"

// MemberAdmission is one uid's worth of the columns that differ between paths.
//
// Everything a path used to set on its own MemberModel before calling
// InsertMemberTx / recoverMemberTx lives here, so that converging a path is a
// mechanical translation rather than a re-derivation. The fields NOT here are
// the ones no path varies: is_deleted (always 0 on admission), bot_admin (reset
// to 0 on restore, defaulted to 0 on insert) and forbidden_expir_time.
type MemberAdmission struct {
	UID string
	// Version is the group-member sequence value. Every path generates one per
	// uid via ctx.GenSeq(common.GroupMemberSeqKey); a missing version breaks
	// incremental member sync, so it is required rather than defaulted.
	Version int64
	// Role is MemberRoleCommon for every admission path today. It is a field
	// rather than a constant because CreateGroup admits its creator.
	Role int
	// InviteUID is the operator that caused the admission.
	InviteUID string
	// Robot mirrors the user row's robot flag.
	Robot int
	// Vercode is the member's invite/verification code. Empty means "generate
	// one" — every path generates the same shape, and the one path that did not
	// write a vercode at all (joinPresetGroups) was defective for it.
	Vercode string
	// IsExternal and SourceSpaceID carry cross-Space external-member display.
	IsExternal    int
	SourceSpaceID string
}

type aiTeamActiveMember struct {
	UID    string `db:"uid"`
	Status int    `db:"status"`
}

// AdmitAITeamContainerMembersTx is the narrow admission bridge used while an
// AI-team container is created or repaired. AI-team provisioning owns the surrounding
// transaction, so calling the public AddGroupMembers service would break the
// atomic group/association/session write. Keeping the write here still routes
// it through the single native membership primitive.
//
// The bridge deliberately accepts exactly one owner and one Bot and verifies
// the freshly-created parent row. It is not a general escape hatch for callers
// outside this package.
func AdmitAITeamContainerMembersTx(
	ctx *config.Context,
	tx *dbr.Tx,
	groupNo, spaceID, ownerUID, botUID string,
) (bool, error) {
	if groupNo == "" || spaceID == "" || ownerUID == "" || botUID == "" || ownerUID == botUID {
		return false, errors.New("group: invalid AI-team container admission")
	}

	var parent struct {
		SpaceID   string `db:"space_id"`
		Creator   string `db:"creator"`
		Purpose   string `db:"purpose"`
		ProjectID string `db:"project_id"`
	}
	count, err := tx.SelectBySql(
		"SELECT space_id,creator,purpose,project_id FROM `group` WHERE group_no=? FOR UPDATE",
		groupNo,
	).Load(&parent)
	if err != nil {
		return false, fmt.Errorf("group: query AI-team container for admission: %w", err)
	}
	if count != 1 || parent.SpaceID != spaceID || parent.Creator != ownerUID ||
		parent.Purpose != aiteampkg.GroupPurpose || parent.ProjectID != "" {
		return false, errors.New("group: AI-team container admission target mismatch")
	}

	var before []*aiTeamActiveMember
	if _, err = tx.SelectBySql(
		"SELECT uid,status FROM group_member WHERE group_no=? AND is_deleted=0 FOR UPDATE",
		groupNo,
	).Load(&before); err != nil {
		return false, fmt.Errorf("group: lock AI-team container members: %w", err)
	}
	wasComplete := hasExactAITeamMembers(before, ownerUID, botUID)
	if wasComplete {
		return false, nil
	}
	ownerVersion, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return false, err
	}
	botVersion, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return false, err
	}

	if err = NewDB(ctx).admitOrRestoreMembersTx(tx, groupNo, []MemberAdmission{
		{
			UID:       ownerUID,
			Version:   ownerVersion,
			Role:      MemberRoleCreator,
			InviteUID: ownerUID,
		},
		{
			UID:       botUID,
			Version:   botVersion,
			Role:      MemberRoleCommon,
			InviteUID: ownerUID,
			Robot:     1,
		},
	}); err != nil {
		return false, err
	}

	var after []*aiTeamActiveMember
	if _, err = tx.SelectBySql(
		"SELECT uid,status FROM group_member WHERE group_no=? AND is_deleted=0 FOR UPDATE",
		groupNo,
	).Load(&after); err != nil {
		return false, fmt.Errorf("group: verify AI-team container members: %w", err)
	}
	if !hasExactAITeamMembers(after, ownerUID, botUID) {
		return false, errors.New("group: AI-team container must contain exactly its owner and Bot")
	}
	return true, nil
}

func hasExactAITeamMembers(members []*aiTeamActiveMember, ownerUID, botUID string) bool {
	if len(members) != 2 {
		return false
	}
	foundOwner, foundBot := false, false
	for _, member := range members {
		if member.Status != int(common.GroupMemberStatusNormal) {
			return false
		}
		switch member.UID {
		case ownerUID:
			foundOwner = true
		case botUID:
			foundBot = true
		default:
			return false
		}
	}
	return foundOwner && foundBot
}

// admitOrRestoreMembersTx is the single native membership write primitive.
//
// It writes the members in ONE statement that decides insert-vs-restore per
// row, and it writes the full column set both branches need.
//
// # Insert vs restore, and why it is one statement
//
// group_member rows are soft-deleted (DeleteMemberTx only sets is_deleted = 1)
// and carry `unique index group_no_uid (group_no, uid)`, so re-joining is an
// UPDATE, not an INSERT. A single upsert covers both first joins and re-joins
// without the old read-then-branch race.
//
// What every path did before was a racy three-statement stand-in: a session-scope
// ExistMemberDelete read, then a branch, then the write. joinPresetGroups fell
// into exactly the race that shape invites — its presence check filtered
// is_deleted = 0, so a uid who had previously left passed the check, the INSERT
// hit the unique index, MySQL returned 1062, the error was logged as a warning,
// and the user was PERMANENTLY never re-added. Two places in this tree already
// document that defect. An upsert cannot have it.
//
// # Assignment order in ON DUPLICATE KEY UPDATE is load-bearing
//
// MySQL evaluates the assignments left to right, so every clause that reads the
// OLD is_deleted must precede the clause that sets it. P0 shipped this exact bug
// in its own admitMemberTx (`updated_at = IF(status = 0, ...)` placed after
// `status = 1`, so a re-admitted member kept the timestamp from their removal)
// and documents it at modules/project/db.go:515-531. The ordering below is not
// stylistic.
//
// # The restore branch reproduces recoverMemberTx exactly
//
// vercode, status, robot and forbidden_expir_time are deliberately NOT touched
// on restore, because recoverMemberTx did not touch them. Three consequences
// worth stating rather than discovering:
//
//   - a restored member keeps their ORIGINAL vercode, so an invite link issued
//     before they left still resolves to them;
//   - a member who was blacklisted (status = 2) and then removed comes back
//     still blacklisted;
//   - forbidden_expir_time survives, which is half of the CheckForbiddenLoop
//     defect — the other half (the poller not filtering is_deleted) is fixed in
//     this change, and DeleteMemberTx now clears the column, so a restore
//     inherits 0 rather than a stale mute.
//
// bot_admin IS reset to 0 on restore, as recoverMemberTx did: a group-granted
// permission must not survive leave-and-rejoin, or a bot's owner can strip and
// re-add it to silently keep bot-admin.
//
// # Re-adding an ALREADY ACTIVE member is a no-op
//
// If the row exists with is_deleted = 0, every conditional assignment declines
// and the statement changes nothing — no version bump, no role reset. That is
// deliberate: the alternative (an unconditional upsert) would DEMOTE a group
// admin who happens to be re-added, and "re-add" is reachable from the preset
// group path on every Space join.
func (d *DB) admitOrRestoreMembersTx(
	tx *dbr.Tx,
	groupNo string,
	admissions []MemberAdmission,
) error {
	if len(admissions) == 0 {
		return nil
	}

	for _, a := range admissions {
		if a.UID == "" {
			return errors.New("group: admission carries an empty uid")
		}
		if a.Version == 0 {
			// A missing version breaks incremental member sync silently — the
			// row exists but no client ever syncs it. Refusing here is how that
			// stops being discoverable only in production.
			return fmt.Errorf("group: admission carries no version for uid %s", a.UID)
		}
	}

	const cols = "(group_no, uid, remark, role, `version`, status, vercode, is_deleted, " +
		"invite_uid, robot, forbidden_expir_time, is_external, source_space_id)"
	placeholders := make([]string, 0, len(admissions))
	args := make([]interface{}, 0, len(admissions)*12)
	for _, a := range admissions {
		vercode := a.Vercode
		if vercode == "" {
			vercode = newMemberVercode()
		}
		placeholders = append(placeholders, "(?, ?, '', ?, ?, ?, ?, 0, ?, ?, 0, ?, ?)")
		args = append(args,
			groupNo, a.UID, a.Role, a.Version, int(common.GroupMemberStatusNormal),
			vercode, a.InviteUID, a.Robot, a.IsExternal, a.SourceSpaceID,
		)
	}

	sql := "INSERT INTO group_member " + cols + " VALUES " +
		strings.Join(placeholders, ", ") +
		" ON DUPLICATE KEY UPDATE " +
		// -- every read of the OLD is_deleted must precede `is_deleted = 0` --
		"  remark          = IF(is_deleted = 1, VALUES(remark), remark), " +
		"  role            = IF(is_deleted = 1, VALUES(role), role), " +
		"  bot_admin       = IF(is_deleted = 1, 0, bot_admin), " +
		"  `version`       = IF(is_deleted = 1, VALUES(`version`), `version`), " +
		"  invite_uid      = IF(is_deleted = 1, VALUES(invite_uid), invite_uid), " +
		"  is_external     = IF(is_deleted = 1, VALUES(is_external), is_external), " +
		"  source_space_id = IF(is_deleted = 1, VALUES(source_space_id), source_space_id), " +
		"  created_at      = IF(is_deleted = 1, NOW(), created_at), " +
		// -- from here on `is_deleted` reads as 0 --
		"  is_deleted      = 0"

	if _, err := tx.InsertBySql(sql, args...).Exec(); err != nil {
		return fmt.Errorf("group: admit or restore members: %w", err)
	}
	return nil
}

// newMemberVercode builds a group-member vercode in the shape every admission
// path used inline. One definition, so a path cannot invent a different shape.
func newMemberVercode() string {
	return fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember)
}

// ---------------------------------------------------------------------------
// D7 — the org-directory listeners are converged, not deleted
// ---------------------------------------------------------------------------

// Legacy org-directory listeners. AddEventListener exists for OrgOrDeptCreate,
// OrgOrDeptEmployeeUpdate and OrgEmployeeExit, and no publisher for any of them
// exists IN THIS REPOSITORY.
//
// The design brief said to delete them on those grounds. Two documents in the
// same .octospec tree disagree about whether this code is dead: #797's inventory
// classifies two of these handlers as 「HR / org-directory offboarding paths —
// arguably the highest-stakes callers」. P1 should not settle that by deleting
// them, for a concrete reason rather than caution: modules/base/event is a
// DATABASE queue, so Wait rows can predate the deploy, and a deleted listener
// drops them silently — an offboarding that never happens, with no error
// anywhere.
//
// So they are routed through the admission funnel like every other path (which
// is mechanical, since they already did the same work by hand), and this counter
// answers the question deletion was supposed to answer. Zero over an observation
// window in production is what makes deleting them a safe, separate change.
const (
	legacyListenerRegisterUser      = "register_user"
	legacyListenerOrgCreate         = "org_or_dept_create"
	legacyListenerOrgEmployeeUpdate = "org_or_dept_employee_update"
	legacyListenerOrgEmployeeExit   = "org_employee_exit"
)

var legacyDirectoryListenerTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: admissionMetricNamespace,
	Name:      "legacy_directory_listener_total",
	Help: "Executions of the org-directory event listeners that have no publisher " +
		"in this repository. Non-zero means they are live and must not be deleted.",
}, []string{"listener"})

func observeLegacyDirectoryListener(listener string) {
	legacyDirectoryListenerTotal.WithLabelValues(listener).Inc()
}

// ---------------------------------------------------------------------------
// Project disband relation metrics
// ---------------------------------------------------------------------------

// Reasons a group left a project. Low-cardinality enum.
const (
	// detachReasonDisband — a human disbanded the project.
	detachReasonDisband = "disband"
	// detachReasonOwnerlessDisband — P0's Space cascade disbanded the project
	// because it had no owner left. Distinguished from a human disband because
	// it means nobody chose this, and a spike in it is worth looking at.
	detachReasonOwnerlessDisband = "ownerless_disband"
)

var projectGroupDetachedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: admissionMetricNamespace,
	Name:      "project_detached_total",
	Help:      "Groups reverted from a Project to Space-direct, by reason.",
}, []string{"reason"})
