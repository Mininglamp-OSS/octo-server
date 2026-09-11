package workspace

import "time"

const (
	WorkspaceStatusArchived = 0
	WorkspaceStatusActive   = 1
)

const (
	MemberStatusInactive = 0
	MemberStatusActive   = 1
)

const (
	MemberRoleMember = 0
	MemberRoleAdmin  = 1
)

const (
	WorkspaceRoleOwner  = "owner"
	WorkspaceRoleAdmin  = "admin"
	WorkspaceRoleMember = "member"
)

// Scope identifies the authenticated actor and an optional, caller-supplied
// Space assertion. Resource methods always derive the authoritative Space from
// the resource itself; ExpectedSpaceID is only an additional consistency check.
type Scope struct {
	UID             string
	ExpectedSpaceID string
}

// Page is the common pagination request. Index is one-based.
type Page struct {
	Index int
	Size  int
}

// Pagination is the common direct pagination response shape.
type Pagination[T any] struct {
	Count int64 `json:"count"`
	List  []T   `json:"list"`
}

// Workspace is the public Workspace representation.
type Workspace struct {
	WorkspaceID   string `json:"workspace_id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Logo          string `json:"logo"`
	OwnerUID      string `json:"owner_uid"`
	SpaceID       string `json:"space_id"`
	MemberCount   int64  `json:"member_count"`
	Status        int    `json:"status"`
	WorkspaceRole string `json:"workspace_role"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// Member is the public Workspace member representation.
type Member struct {
	UID           string `json:"uid"`
	Name          string `json:"name"`
	WorkspaceRole string `json:"workspace_role"`
	Status        int    `json:"status"`
	CreatedAt     string `json:"created_at"`
	GrantedBy     string `json:"granted_by"`
}

// CreateRequest is the input accepted by Create.
type CreateRequest struct {
	SpaceID     string `json:"space_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
}

// UpdateRequest is a partial Workspace metadata update.
type UpdateRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Logo        *string `json:"logo"`
}

// MemberInput is one member in an AddMembers batch.
type MemberInput struct {
	UID           string `json:"uid"`
	WorkspaceRole string `json:"workspace_role"`
}

// MemberFilter controls member list filtering. Status is 1 by default; both
// statuses are valid when explicitly selected.
type MemberFilter struct {
	Roles  []string
	Status int
}

// Access is the transactionally-authorized view used by other modules. Role is
// the actor's projected Workspace role.
type Access struct {
	WorkspaceID string
	SpaceID     string
	OwnerUID    string
	Role        string
	MemberUIDs  []string
}

// SpaceSeatKey identifies one exact Space membership row. Group snapshot
// callers must prepare and de-duplicate these keys before opening a database
// transaction.
type SpaceSeatKey struct {
	SpaceID string
	UID     string
}

// GroupAccess is the authorization and active-member snapshot returned to a
// group-creation caller. It deliberately exposes only the facts required by
// that workflow.
type GroupAccess struct {
	WorkspaceID        string
	SpaceID            string
	OwnerUID           string
	Role               string
	MemberUIDs         []string
	EligibleMemberUIDs []string
}

type workspaceModel struct {
	ID          int64     `db:"id"`
	WorkspaceID string    `db:"workspace_id"`
	SpaceID     string    `db:"space_id"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	Logo        string    `db:"logo"`
	OwnerUID    string    `db:"owner_uid"`
	Status      int       `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

type workspaceMemberModel struct {
	WorkspaceID string    `db:"workspace_id"`
	UID         string    `db:"uid"`
	Role        int       `db:"role"`
	Status      int       `db:"status"`
	GrantedBy   string    `db:"granted_by"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

type memberNameModel struct {
	UID  string `db:"uid"`
	Name string `db:"name"`
}

const workspaceTimeFormat = "2006-01-02 15:04:05"

func formatWorkspaceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format(workspaceTimeFormat)
}

// InternalWorkspace is the service-facing Workspace representation. It does
// not contain workspace_role because internal callers are not Workspace
// members and therefore have no caller-relative role projection.
type InternalWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	SpaceID     string `json:"space_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
	OwnerUID    string `json:"owner_uid"`
	MemberCount int64  `json:"member_count"`
	Status      int    `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func internalWorkspacePublic(row *workspaceModel, memberCount int64) *InternalWorkspace {
	if row == nil {
		return nil
	}
	return &InternalWorkspace{
		WorkspaceID: row.WorkspaceID,
		SpaceID:     row.SpaceID,
		Name:        row.Name,
		Description: row.Description,
		Logo:        row.Logo,
		OwnerUID:    row.OwnerUID,
		MemberCount: memberCount,
		Status:      row.Status,
		CreatedAt:   formatWorkspaceTime(row.CreatedAt),
		UpdatedAt:   formatWorkspaceTime(row.UpdatedAt),
	}
}
