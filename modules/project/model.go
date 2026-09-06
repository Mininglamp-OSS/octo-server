package project

import "time"

// ---------- enums ----------

// Project status (octo_project.status).
const (
	// StatusDisbanded — the project has been disbanded. Terminal: its name is
	// released (the active_name generated column goes NULL) and every read path
	// treats it as nonexistent.
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
// Named for what it is. These values filter the Space project list and directory
// search; they are NOT a security boundary — a Space admin can still enumerate
// project metadata. Calling the field "visibility" or "secret" would invite
// readers to treat it as isolation, which it is not.
const (
	// DiscoverabilitySpaceListed — appears in the Space project list.
	DiscoverabilitySpaceListed = 0
	// DiscoverabilityUnlisted — hidden from the list; reachable by its members
	// and by Space admins.
	DiscoverabilityUnlisted = 1
)

// IsValidDiscoverability reports whether d is a known discoverability value.
func IsValidDiscoverability(d int) bool {
	return d == DiscoverabilitySpaceListed || d == DiscoverabilityUnlisted
}

// Join modes (octo_project.join_mode).
//
// P0 has NO self-service join path — the invite surface is P2 — so this column
// has no consumer yet. It ships now for the same reason is_official does: it is a
// column on octo_project, and adding it later means a second migration against a
// table that by then carries production traffic.
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
	ID              int64     `db:"id"`
	ProjectID       string    `db:"project_id"`
	SpaceID         string    `db:"space_id"`
	Name            string    `db:"name"`
	Description     string    `db:"description"`
	Logo            string    `db:"logo"`
	Creator         string    `db:"creator"`
	Discoverability int       `db:"discoverability"`
	JoinMode        int       `db:"join_mode"`
	MaxMembers      int       `db:"max_members"`
	MemberEpoch     int64     `db:"member_epoch"`
	Status          int       `db:"status"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}

// MemberModel is an octo_project_member row.
type MemberModel struct {
	ProjectID string    `db:"project_id"`
	UID       string    `db:"uid"`
	SpaceID   string    `db:"space_id"`
	Role      int       `db:"role"`
	Status    int       `db:"status"`
	InviteUID string    `db:"invite_uid"`
	CreatedAt time.Time `db:"created_at"`
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
	Name            string `json:"name"`
	Description     string `json:"description"`
	Logo            string `json:"logo"`
	Discoverability *int   `json:"discoverability"`
	JoinMode        *int   `json:"join_mode"`
	MaxMembers      *int   `json:"max_members"`
}

type updateReq struct {
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	Logo            *string `json:"logo"`
	Discoverability *int    `json:"discoverability"`
	JoinMode        *int    `json:"join_mode"`
	MaxMembers      *int    `json:"max_members"`
}

type membersReq struct {
	UIDs []string `json:"uids"`
}

type leaveReq struct {
	// TransferTo names the successor when the caller is the last owner. Leaving
	// without it is rejected rather than silently producing an ownerless project.
	TransferTo string `json:"transfer_to"`
}

type roleReq struct {
	Role int `json:"role"`
	// TransferTo is required when demoting the last owner, for the same reason as
	// in leaveReq.
	TransferTo string `json:"transfer_to"`
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
	JoinMode        int    `json:"join_mode"`
	MaxMembers      int    `json:"max_members"`
	MemberCount     int    `json:"member_count"`
	MemberEpoch     int64  `json:"member_epoch"`
	Status          int    `json:"status"`
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
	CreatedAt string `json:"created_at"`
}

// memberRosterModel is the member roster joined to `user` for display names.
type memberRosterModel struct {
	MemberModel
	Name string `db:"name"`
}

const respTimeFormat = "2006-01-02 15:04:05"

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(respTimeFormat)
}
