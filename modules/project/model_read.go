package project

import "database/sql"

// projectReadPage is the bounded page requested by a project list or roster
// read. The read service receives normalized values so count and page queries
// cannot accidentally use different limits.
type projectReadPage struct {
	Offset int64
	Limit  int64
}

type projectArchiveFilter uint8

const (
	projectArchiveActive projectArchiveFilter = iota
	projectArchiveOnly
	projectArchiveAll
)

type archiveNoticeReadRow struct {
	ProjectID      string       `db:"project_id"`
	Name           string       `db:"name"`
	ArchiveVersion int64        `db:"archive_version"`
	ArchivedAt     sql.NullTime `db:"archived_at"`
}

type archiveNoticeResp struct {
	ProjectID      string `json:"project_id"`
	Name           string `json:"name"`
	ArchiveVersion int64  `json:"archive_version"`
	ArchivedAt     string `json:"archived_at"`
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
type projectArchiveNoticeResult struct {
	Rows  []*archiveNoticeReadRow
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

// MemberReadPage is the bounded roster page accepted by the cross-module service.
type MemberReadPage struct {
	Offset int64
	Limit  int64
}

// MemberListResult exposes the existing wire projection, not internal database rows.
type MemberListResult struct {
	Members []*MemberResp
	Total   int64
}
