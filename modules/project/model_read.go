package project

// projectReadPage is the bounded page requested by a project list or roster
// read. The read service receives normalized values so count and page queries
// cannot accidentally use different limits.
type projectReadPage struct {
	Offset int64
	Limit  int64
}

// projectReadListRow carries project data plus caller-relative list fields.
type projectReadListRow struct {
	Model
	MyRole int `db:"my_role"`
	Pinned int `db:"pinned"`
	Humans int
	Agents int
}

// projectReadMemberRow carries a member seat and display metadata loaded from
// the legacy user and robot tables in separate reads. Keeping those reads
// separate avoids relying on cross-schema collation compatibility.
type projectReadMemberRow struct {
	MemberModel
	Name     string
	Robot    int
	OwnerUID string
	Roles    []CollaborationRoleResp
}

type projectReadListResult struct {
	Rows  []*projectReadListRow
	Total int64
}

type projectReadMembersResult struct {
	Rows  []*projectReadMemberRow
	Total int64
}

type projectReadProjectResult struct {
	Project   *Model
	Role      int
	SpaceRole int
	Humans    int
	Agents    int
	Pinned    bool
}
