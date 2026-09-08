package project

import (
	"database/sql"
	"time"
)

// ---------- enums ----------

// Project status (octo_project.status).
const (
	// StatusDisbanded — the project has been disbanded. Read paths treat it
	// as nonexistent; project names are independent labels.
	StatusDisbanded = 0
	// StatusNormal — active.
	StatusNormal = 1
)

// Member status (octo_project_member.status). Rows are never deleted, so
// re-adding a member is a status flip rather than an INSERT that a unique index
// could reject.
const (
	// MemberStatusRemoved — the seat is gone but the row stays for audit.
	MemberStatusRemoved = 0
	// MemberStatusActive — an active seat. I1 constrains exactly these rows.
	MemberStatusActive = 1
)

// Member roles (octo_project_member.role). Ordered, and the ordering is
// load-bearing: the transitive-protection rules compare roles numerically.
const (
	// RoleCommon — ordinary project member.
	RoleCommon = 0
	// RoleAdmin — may edit the project and manage ordinary members.
	RoleAdmin = 1
	// RoleOwner — may additionally disband and change roles.
	RoleOwner = 2
)

// roleNonMember is the sentinel the middleware writes for a caller who is not a
// project member. It is deliberately negative: RoleCommon is 0, which is also
// the zero value of an int, so a "not a member" state spelled as 0 would grant
// ordinary-member rights on any code path that forgot to check membership.
const roleNonMember = -1

// IsValidRole reports whether r is an assignable role.
func IsValidRole(r int) bool { return r == RoleCommon || r == RoleAdmin || r == RoleOwner }

// Discoverability (octo_project.discoverability).
//
// Project discoverability is retained for storage compatibility; all Project
// reads still require an active Space identity and active Project membership.
const (
	// DiscoverabilitySpaceListed — legacy display classification.
	DiscoverabilitySpaceListed = 0
	// DiscoverabilityUnlisted — legacy display classification.
	DiscoverabilityUnlisted = 1
)

// IsValidDiscoverability reports whether d is a known discoverability value.
func IsValidDiscoverability(d int) bool {
	return d == DiscoverabilitySpaceListed || d == DiscoverabilityUnlisted
}

// Join modes (octo_project.join_mode).
//
// P0 has NO self-service join path — the invite surface is P2 — so the column exists with
// its DDL default and NOTHING above the storage layer reads or writes it. Same treatment as
// is_official: no model field, no request field, no response field. A client-visible
// join_mode with no enforcement point would let deployments persist join_mode=0 today and
// hand every such row open-join semantics the day the P2 path lands, with nobody
// re-consenting (yujiawei S-2, PR #841).
const (
	// JoinModeOpen — any Space member may join without an invite (P2).
	JoinModeOpen = 0
	// JoinModeInviteOnly — admission is by invite or admin add. P0 default.
	JoinModeInviteOnly = 1
)

// ---------- DB models ----------

// Model is an octo_project row.
//
// Deliberately has NO ActiveName field. active_name is a STORED generated column,
// and MySQL rejects any INSERT/UPDATE that names it with error 3105. The DAO uses
// explicit column lists rather than util.AttrToUnderscore, so this is belt and
// braces rather than the only guard — but a struct field would make the failure
// reachable again the moment someone reaches for the reflective helper.
//
// It likewise has no IsOfficial field: no P0 code path writes that column, and
// leaving it out of the model is what makes that checkable rather than aspirational.
type Model struct {
	ID              int64  `db:"id"`
	ProjectID       string `db:"project_id"`
	SpaceID         string `db:"space_id"`
	Name            string `db:"name"`
	Description     string `db:"description"`
	Logo            string `db:"logo"`
	Creator         string `db:"creator"`
	Discoverability int    `db:"discoverability"`
	MaxMembers      int    `db:"max_members"`
	MemberEpoch     int64  `db:"member_epoch"`
	// CollaborationRoleEpoch orders statements about the project's collaboration-role
	// catalog. Third counter on this row, same increment-only discipline (from #871).
	CollaborationRoleEpoch int64 `db:"collaboration_role_epoch"`
	// LifecycleVersion orders statements about the PROJECT — its name, its
	// description, whether it is active. Deliberately separate from MemberEpoch,
	// which orders statements about the ROSTER: one shared counter would make
	// every rename invalidate every cached authorization decision downstream, and
	// every membership change look like a lifecycle statement. They move at
	// different rates for different reasons.
	LifecycleVersion int64 `db:"lifecycle_version"`
	Status           int   `db:"status"`
	// AllMemberGroupNo is this project's all-member group, or "" when it has
	// none yet. "" is the sentinel and the column is NOT NULL, so every
	// predicate in the feature is written `= ''` / `!= ''` (see D5).
	//
	// Empty is a REACHABLE state, not an error: the group is provisioned after
	// the create transaction commits (the hook opens its own transaction in
	// modules/group), so a provisioning failure leaves the project alive with no
	// group. D4 makes that recoverable rather than terminal — the next write path
	// on this project retries under a lease, and reconcile scan A reports it.
	AllMemberGroupNo string `db:"all_member_group_no"`
	// ActivatedAt is the two-phase create latch: when the subsystem side
	// confirmed this project has a container. NULL until it does, and the two
	// peer-facing endpoints answer about a NULL row exactly as they answer about
	// a project that does not exist.
	//
	// sql.NullTime rather than *time.Time, and the difference is not stylistic:
	// dbr materializes a NULL column into a NON-NIL *time.Time at the zero time,
	// so a pointer field would read as "activated in year zero" for every
	// unconfirmed project and every `if m.ActivatedAt != nil` would be true. A
	// test helper written the pointer way did exactly that before this comment
	// existed. NullTime carries the NULL in a field of its own, outside the value
	// domain — the same reason the epoch sentinel had to leave it.
	ActivatedAt sql.NullTime `db:"activated_at"`
	CreatedAt   time.Time    `db:"created_at"`
	UpdatedAt   time.Time    `db:"updated_at"`
}

// MemberModel is an octo_project_member row.
type MemberModel struct {
	ProjectID string `db:"project_id"`
	UID       string `db:"uid"`
	SpaceID   string `db:"space_id"`
	Role      int    `db:"role"`
	Status    int    `db:"status"`
	// Removing is D4's seat-closing flag: 1 means the seat is being torn down
	// while Status is still MemberStatusActive.
	//
	// Every authorization read treats Removing == 1 as a NON-member — the member
	// list and middleware's role resolution use this clause. Status stays active
	// until the removal worker finishes registered cleanup; native group membership
	// is independent and is not mutated by this Project-side seat close.
	Removing  int       `db:"removing"`
	InviteUID string    `db:"invite_uid"`
	CreatedAt time.Time `db:"created_at"`
	// JoinedAt is the start of the current active membership round. During the
	// rolling expand window legacy writers may leave it NULL; every read projection
	// uses COALESCE(joined_at, created_at) before scanning this time.Time field.
	// New admissions initialize it with CreatedAt and re-admissions refresh it.
	JoinedAt  time.Time `db:"joined_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// officialFlagModel reads is_official back for the D6 guard test. It exists only
// so the assertion "no P0 path ever writes is_official" can be made against the
// real table without putting the column on the write-side model.
type officialFlagModel struct {
	ProjectID  string `db:"project_id"`
	IsOfficial int    `db:"is_official"`
}

// ---------- API request payloads ----------

type createReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
	// AgentUIDs are the caller's OWN AI agents to seat in the new project
	// (D2/D15). Optional; the eligibility of every uid here is re-decided
	// server-side inside the create transaction, never trusted from the client.
	//
	// One ineligible uid rejects the WHOLE request (D3): "the project was
	// created but two of your agents are missing" is harder to explain than
	// "fix it and retry", and a partial success would add a third meaning to
	// D4's already-loaded failure story.
	AgentUIDs       []string `json:"agent_uids"`
	Discoverability *int     `json:"discoverability"`
	MaxMembers      *int     `json:"max_members"`
}

type updateReq struct {
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	Logo            *string `json:"logo"`
	Discoverability *int    `json:"discoverability"`
	MaxMembers      *int    `json:"max_members"`
}

// settingReq is the caller's personal preferences for one project.
//
// Shaped as a settings bag with pointer fields rather than as /pin and /unpin
// routes, following PUT /v1/groups/:group_no/setting, which carries top / mute /
// save / remark through one endpoint. Two verb routes look simpler while there is
// one preference and stop looking simpler at the second: a preference then costs a
// key here, not two routes and two handlers.
//
// A pointer distinguishes "not mentioned" from "set to false", so a client that
// learns about a future preference does not have to send every field to change
// one. A body that names no preference (`{}`) is a no-op, not a reset — a
// zero-byte body is a 400, see updateSettingHandler.
type settingReq struct {
	Pinned *bool `json:"pinned"`
}

// memberAdd is the per-target role contract for an atomic members/add batch.
// Role 0 (member) is the default when omitted; RoleOwner is never accepted
// here because owner changes have a dedicated atomic transfer endpoint.
type memberAdd struct {
	UID  string `json:"uid"`
	Role int    `json:"role"`
}

type membersReq struct {
	Members []memberAdd `json:"members"`
}

type memberUIDsReq struct {
	UIDs []string `json:"uids"`
}

type ownerTransferReq struct {
	UID string `json:"uid"`
}

type roleReq struct {
	// Role is a POINTER so a payload that names no role is distinguishable from one
	// naming RoleCommon. As a plain int, `{}` and `{"role": null}` both decoded to 0,
	// passed IsValidRole, and silently DEMOTED the target with a 200 — a destructive
	// action as the failure mode of a broken payload, which is the same thing the leave
	// handler was hardened against in round 1. Matches updateReq, where every optional
	// field is a pointer for the same reason.
	Role *int `json:"role"`
}

type collaborationRoleNameReq struct {
	Name string `json:"name"`
}

type collaborationRoleBindingReq struct {
	RoleIDs []string `json:"role_ids"`
}

// ---------- API responses ----------

// Resp is the Project payload returned by list and detail.
//
// MemberEpoch ships here and nowhere else in P0 (D3): first-party clients get it
// next to my_role and the capability bits, but no machine-to-machine endpoint
// exposes it. This repo has already built "an endpoint and waited for a consumer"
// twice, and the eventual subsystem channel is verify?include=context, not a new
// route.
//
// Capabilities are emitted as explicit booleans rather than left for the client to
// derive from MyRole. A client that computes permissions from a role number
// re-implements the server's permission matrix, and the two drift the first time
// the matrix changes.
type Resp struct {
	ProjectID       string `json:"project_id"`
	SpaceID         string `json:"space_id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Logo            string `json:"logo"`
	Creator         string `json:"creator"`
	Discoverability int    `json:"discoverability"`
	MaxMembers      int    `json:"max_members"`
	// MemberCount is EVERY active seat, humans and agents together — the same
	// meaning it had before D16, and the same meaning `member_count` carries in
	// modules/opanalytics. The split lives in the two fields below.
	//
	// D16 first shipped this as "humans only", on the argument that a project with
	// one person and two of their agents rendering as "3 人" is a sentence no user
	// reads as true. That argument is right about what a client should DISPLAY and
	// wrong about which field should change: the display fix is for the client to
	// render HumanMemberCount. Redefining member_count bought the same outcome at
	// the price of one server meaning two things by one name —
	// modules/opanalytics' channel list already ships member_count as the total,
	// beside human_member_count and agent_member_count, to the same client teams.
	//
	// Restored while it is still free, and the reason is checkable from the tags
	// rather than from a claim about GA. v1.18.0 (2026-09-07) already ships this
	// field from countActiveMembers -- the TOTAL. The humans-only redefinition
	// landed in 566e625, which `git tag --contains` places on no release. So this
	// restores the meaning that IS released and retires the one that never was: a
	// v1.18.0 client sees no change at all, and only a client built against two
	// days of unreleased main renames a field.
	//
	// The earlier version of this comment said the module "has never been GA, so
	// nothing had shipped against either meaning". That was wrong -- v1.18.0
	// carries 42 files under modules/project -- and it was the weaker argument
	// besides. PR #868's fifth review, which checked the tags instead of taking
	// the sentence. This is the sentence a future wire change will cite, so it
	// needs to rest on something measurable.
	//
	// The quota (MaxMembers) bounds this number, agents included — an agent reads
	// the project's messages, so it costs a seat.
	MemberCount int `json:"member_count"`
	// HumanMemberCount counts human members only. This is what a roster header
	// should render.
	HumanMemberCount int `json:"human_member_count"`
	// AgentMemberCount counts active AI agent seats.
	//
	// Named for opanalytics' field rather than the shorter `agent_count`, which is
	// already taken on the wire by the Space directory, where it means "how many
	// agents this OWNER has" — a different question with the same short name.
	AgentMemberCount       int   `json:"agent_member_count"`
	MemberEpoch            int64 `json:"member_epoch"`
	CollaborationRoleEpoch int64 `json:"collaboration_role_epoch"`
	Status                 int   `json:"status"`
	// AllMemberGroupNo is this project's all-member group, or "" when it has
	// none yet (provisioning failed and has not been retried; see D4). A client
	// showing an entry point to the group must handle "" rather than assuming.
	AllMemberGroupNo string `json:"all_member_group_no"`
	// Pinned is the CALLER's own pin, not a property of the project: the same
	// project reads true for one user and false for the next. It is on the list
	// AND the detail route, because a field present on one and absent on the other
	// makes the two disagree about the same project — the defect
	// all_member_group_no already had to be fixed for once.
	Pinned bool `json:"pinned"`
	// MyRole is the caller's project role, or -1 when the caller is not a member
	// (a Space admin reading a project they have not joined).
	MyRole       int          `json:"my_role"`
	Capabilities Capabilities `json:"capabilities"`
	CreatedAt    string       `json:"created_at"`
	UpdatedAt    string       `json:"updated_at"`
}

// Capabilities is the server's verdict on what the caller may do with this
// project, so clients never derive permissions from a role number.
type Capabilities struct {
	CanUpdate       bool `json:"can_update"`
	CanDisband      bool `json:"can_disband"`
	CanManageMember bool `json:"can_manage_member"`
	CanChangeRole   bool `json:"can_change_role"`
	CanLeave        bool `json:"can_leave"`
	CanViewMembers  bool `json:"can_view_members"`
}

// MemberResp is one row of the project member roster.
type MemberResp struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Role      int    `json:"role"`
	InviteUID string `json:"invite_uid"`
	// Robot is 1 for an AI agent seat, 0 for a person (D16). Without it the
	// roster cannot be rendered the way the directory renders it — agents
	// nested under the person who owns them — and a client would have to guess
	// from the uid shape.
	Robot int `json:"robot"`
	// OwnerUID is the agent's owner (robot.creator_uid); empty for a person and
	// for an agent whose owner row is gone. It is the join key the client uses
	// to nest an agent under its owner.
	OwnerUID           string                  `json:"owner_uid"`
	CollaborationRoles []CollaborationRoleResp `json:"collaboration_roles"`
	CreatedAt          string                  `json:"created_at"`
	JoinedAt           string                  `json:"joined_at"`
}

const (
	CollaborationRoleSourceBuiltin = "builtin"
	CollaborationRoleSourceCustom  = "custom"
)

type CollaborationRoleModel struct {
	RoleID         string    `db:"role_id"`
	ProjectID      string    `db:"project_id"`
	BuiltinKey     string    `db:"builtin_key"`
	Name           string    `db:"name"`
	NormalizedName string    `db:"normalized_name"`
	Source         string    `db:"source"`
	CreatorUID     string    `db:"creator_uid"`
	CreatedAt      time.Time `db:"created_at"`
	UpdatedAt      time.Time `db:"updated_at"`
}

type CollaborationRoleResp struct {
	RoleID     string `json:"role_id"`
	BuiltinKey string `json:"builtin_key,omitempty"`
	Name       string `json:"name"`
	Source     string `json:"source"`
}

type collaborationRoleCatalogResp struct {
	CollaborationRoleEpoch int64                   `json:"collaboration_role_epoch"`
	Roles                  []CollaborationRoleResp `json:"roles"`
}

const respTimeFormat = "2006-01-02 15:04:05"

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(respTimeFormat)
}
