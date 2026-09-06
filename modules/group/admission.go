package group

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Group-membership admission — the single entry every path goes through.
//
// # Why this file exists
//
// Invariant I2 — for a group whose project_id is not the empty sentinel, every
// active group_member row belongs to an active member of that project (system
// bots exempted) — is the ENTIRE security mechanism of the Project layer. There
// is no read-path filter behind it: a uid with a group_member row sees the group
// in /v1/sidebar/sync, receives its messages over WuKongIM, and can post. So one
// missed admission path is not a cosmetic bug, it is the whole hole.
//
// Before this file, group membership was admitted by ELEVEN distinct code paths
// with no shared funnel: two near-duplicate implementations, nine sites calling
// the DAO primitives directly, and two doing raw DML from outside modules/group
// entirely. The invariant they shared was maintained by hand-copied comments
// (「与 Service.AddGroupMembers 保持一致」 appears five times in this module).
// P0's journal recorded what that costs: its own I1 checks were added one review
// round at a time, each round finding another path.
//
// The counter-move, and what this file is:
//
//  1. one admission entry every path goes through (admitOrRestoreMembersTx),
//  2. a source guard that fails CI when a new path writes group_member outside
//     the allowlist (admission_guard_test.go),
//  3. a per-entry-point rejection metric, so a path that silently stopped
//     enforcing shows up in production rather than in the next review,
//  4. a reconcile scan reporting violations that got in anyway (modules/project).
//
// Items 2 and 3 are not decoration. A funnel with no guard is a convention, and
// a convention is what the five copied comments already were.
const admissionMetricNamespace = "group"

// Admission entry points. Every path that can put a uid into a group's active
// member set has a label here, and the acceptance requires the test suite to
// emit every one of them at least once: a label that is never emitted is a path
// that is not enforcing.
//
// The numbering follows the task brief's inventory so the two can be read
// side by side. A9 (IService.AddMember) has no label because it was DELETED
// rather than converged — it had zero non-test callers, no transaction, no Space
// check, and no version, and it was exported on IService, so any future module
// could have bypassed every gate through it.
const (
	// AdmissionEntryInviteConfirm is addMembersTxWithSpace, reached from
	// groupMemberInviteSure — mounted on the openGroup route group with NO
	// AuthMiddleware. The most exposed admission path in the product.
	AdmissionEntryInviteConfirm = "a1_invite_confirm"
	// AdmissionEntryAddMembers is Service.AddGroupMembers.
	AdmissionEntryAddMembers = "a2_add_members"
	// AdmissionEntryCreateGroup is CreateGroup's initial member list.
	AdmissionEntryCreateGroup = "a3_create_group"
	// AdmissionEntryCreateGroupBot is CreateGroup's req.BotUID.
	AdmissionEntryCreateGroupBot = "a4_create_group_bot"
	// AdmissionEntryScanJoin is groupScanJoin (QR-code join).
	AdmissionEntryScanJoin = "a5_scan_join"
	// AdmissionEntryRegisterUser is handleRegisterUserEvent.
	AdmissionEntryRegisterUser = "a6_register_user"
	// AdmissionEntryOrgCreate is handleOrgOrDeptCreateEvent. Converged, not
	// deleted: no publisher exists in this repository, but the event table is a
	// database queue whose Wait rows can predate the deploy, and #797 classifies
	// these handlers as org-directory offboarding paths. See D7.
	AdmissionEntryOrgCreate = "a7_org_create"
	// AdmissionEntryOrgEmployeeUpdate is handleOrgOrDeptEmployeeUpdate's add branch.
	AdmissionEntryOrgEmployeeUpdate = "a8_org_employee_update"
	// AdmissionEntryPresetGroups is modules/space's joinPresetGroups, reached
	// through the registered admitter because modules/space cannot import
	// modules/group (group already imports space).
	AdmissionEntryPresetGroups = "a10_preset_groups"
	// AdmissionEntryUnblacklist is the un-blacklist branch of the blacklist
	// handler.
	//
	// This one is the reason a gate installed only in the two admission
	// primitives is not enough. It restores a uid to the active member set by
	// flipping group_member.status back to Normal, re-subscribes them to the IM
	// channel and to the group's threads, and touches NEITHER InsertMemberTx nor
	// recoverMemberTx. It is an admission path because that is what it does.
	AdmissionEntryUnblacklist = "a11_unblacklist"
)

// Rejection reasons. Low-cardinality enum; never a free-form message.
const (
	admissionReasonNotProjectMember = "not_project_member"
	admissionReasonNotSpaceMember   = "not_space_member"
)

// admissionRejectedTotal counts refused admissions by entry point.
//
// The breakdown IS the metric. A single undifferentiated counter cannot tell you
// that one of eleven paths stopped enforcing; a per-entry breakdown can, because
// a path that was converted and then regressed goes silent while its siblings
// keep counting.
//
// No group_no / project_id / uid labels: unbounded cardinality.
var admissionRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: admissionMetricNamespace,
	Name:      "admission_rejected_total",
	Help:      "Group-membership admissions refused, by entry point and reason.",
}, []string{"entry", "reason"})

// ErrAdmissionRefused is the sentinel every admission refusal wraps, so callers
// can distinguish "this uid may not be admitted" from "the database broke"
// without string matching.
var ErrAdmissionRefused = errors.New("group: admission refused")

// AdmissionRefusedError names the uids that were refused and why.
//
// It carries the uids because the batch paths must be able to tell the operator
// WHICH member of a 200-uid batch was the problem. It does NOT reach the wire in
// that shape: handlers map it to a registered i18n code, and the uid list goes
// to the log.
type AdmissionRefusedError struct {
	Entry  string
	Reason string
	UIDs   []string
}

func (e *AdmissionRefusedError) Error() string {
	return fmt.Sprintf("group: admission refused at %s (%s): %s",
		e.Entry, e.Reason, strings.Join(e.UIDs, ","))
}

func (e *AdmissionRefusedError) Unwrap() error { return ErrAdmissionRefused }

// assertAdmissibleTx is THE composite gate: may these uids join this group?
//
// The predicate is
//
//	active Space member AND (project is the empty sentinel OR active project member)
//
// evaluated for every uid, and it is deliberately a CONJUNCTION. P0's
// Space→Project cascade is asynchronous — a 10-second poller, a 10-minute lease,
// backoff, a terminal abandoned state — so between a Space removal committing
// and that cascade running, octo_project_member still holds status = 1 rows for
// a uid with no Space seat. A gate that asked only "is this an active project
// member?" would admit a removed Space member into a project group for the whole
// of that window. With the conjunction the window costs nothing: the Space half
// fails first. P0's space_member_removal.go says the tolerance for that window
// "expires in P1"; this is where it expires.
//
// # Space-direct groups pay NOTHING for this (C1)
//
// projectID == "" returns before issuing any query at all. That is a hard
// requirement, not an optimization: this gate is on the admission path of every
// group in the product, and a gate that runs and passes is still a latency
// regression on every group join. The short-circuit is asserted by a test that
// COUNTS queries, because "I read the code and it returns early" is not evidence.
//
// # Why the Space half is a session read and the project half is not
//
// The Space half calls pkg/space.ActiveMembers on a *dbr.Session — outside the
// caller's transaction, exactly as all eleven paths already did individually. It
// therefore proves nothing about state at COMMIT time. That weakness is
// pre-existing and deliberately UNCHANGED here: tightening it into the
// transaction is a strict improvement and a behaviour change on every group join
// in the product, so it is a separate task, not a rider on this one.
//
// The project half must not copy it. pkg/project.AssertMembersInProjectTx takes
// the caller's *dbr.Tx and a shared lock on the member rows, so a concurrent
// project-seat removal cannot commit between the check and the admission. A
// check outside the transaction is a TOCTOU with a comment.
func (d *DB) assertAdmissibleTx(tx *dbr.Tx, spaceID, projectID string, uids []string, entry string) error {
	// C1 — no query, no allocation, nothing, for a Space-direct group.
	if projectID == "" || len(uids) == 0 {
		return nil
	}

	// Space half. Only project groups reach it, so this adds no cost to the
	// paths that carry the product's traffic today.
	if spaceID != "" {
		active, err := spacepkg.ActiveMembers(d.ctx.DB(), spaceID, uids)
		if err != nil {
			return fmt.Errorf("group: admission space check: %w", err)
		}
		missing := make([]string, 0)
		for _, uid := range uids {
			// System bots are exempt from the PROJECT half by whitelist, but not
			// from the Space half: a bot in a Space group is a Space member.
			// Keeping them subject to it means the exemption widens exactly one
			// gate, not two.
			if uid != "" && !active[uid] {
				missing = append(missing, uid)
			}
		}
		if len(missing) > 0 {
			admissionRejectedTotal.WithLabelValues(entry, admissionReasonNotSpaceMember).
				Add(float64(len(missing)))
			return &AdmissionRefusedError{
				Entry:  entry,
				Reason: admissionReasonNotSpaceMember,
				UIDs:   missing,
			}
		}
	}

	// Project half — in-transaction, locked.
	missing, err := projectpkg.AssertMembersInProjectTx(tx, projectID, uids)
	if err != nil {
		return fmt.Errorf("group: admission project check: %w", err)
	}
	if len(missing) > 0 {
		admissionRejectedTotal.WithLabelValues(entry, admissionReasonNotProjectMember).
			Add(float64(len(missing)))
		return &AdmissionRefusedError{
			Entry:  entry,
			Reason: admissionReasonNotProjectMember,
			UIDs:   missing,
		}
	}
	return nil
}

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

// admitOrRestoreMembersTx is the single admission entry. Every path that adds a
// uid to a group's active member set goes through it.
//
// It enforces the composite gate, then writes the members in ONE statement that
// decides insert-vs-restore per row, and it writes the full column set both
// branches need.
//
// # Insert vs restore, and why it is one statement
//
// group_member rows are soft-deleted (DeleteMemberTx only sets is_deleted = 1)
// and carry `unique index group_no_uid (group_no, uid)`, so re-joining is an
// UPDATE, not an INSERT. A gate installed only in InsertMemberTx would cover
// first joins and miss every rejoin.
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
	groupNo, spaceID, projectID string,
	admissions []MemberAdmission,
	entry string,
) error {
	if len(admissions) == 0 {
		return nil
	}

	uids := make([]string, 0, len(admissions))
	for _, a := range admissions {
		if a.UID == "" {
			return fmt.Errorf("group: admission at %s carries an empty uid", entry)
		}
		if a.Version == 0 {
			// A missing version breaks incremental member sync silently — the
			// row exists but no client ever syncs it. Refusing here is how that
			// stops being discoverable only in production.
			return fmt.Errorf("group: admission at %s carries no version for uid %s", entry, a.UID)
		}
		uids = append(uids, a.UID)
	}

	if err := d.assertAdmissibleTx(tx, spaceID, projectID, uids, entry); err != nil {
		return err
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
		return fmt.Errorf("group: admit or restore members at %s: %w", entry, err)
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
// Project cascade metrics
// ---------------------------------------------------------------------------

// Reasons a group left a project. Low-cardinality enum.
const (
	// detachReasonDisband — a human disbanded the project.
	detachReasonDisband = "disband"
	// detachReasonOwnerlessDisband — P0's Space cascade disbanded the project
	// because it had no owner left. Distinguished from a human disband because
	// it means nobody chose this, and a spike in it is worth looking at.
	detachReasonOwnerlessDisband = "ownerless_disband"
	// detachReasonNoSuccessor — a group's creator left the project and no
	// remaining member of that group was still in the project, so the group fell
	// back to Space-direct rather than being force-transferred or disbanded.
	detachReasonNoSuccessor = "no_successor"
)

var projectGroupDetachedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: admissionMetricNamespace,
	Name:      "project_detached_total",
	Help:      "Groups reverted from a Project to Space-direct, by reason.",
}, []string{"reason"})

// projectGroupHandoverTotal counts ownership handovers performed by the cascade.
//
// Worth its own counter rather than a log line: it is the one place the system
// changes who controls a group without anyone asking, and the product decision
// to do that automatically is only defensible while the number stays small.
var projectGroupHandoverTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: admissionMetricNamespace,
	Name:      "project_cascade_handover_total",
	Help:      "Group ownership handovers performed because the creator left the project.",
})
