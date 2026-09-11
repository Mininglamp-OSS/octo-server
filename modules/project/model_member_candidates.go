package project

const (
	memberCandidateStatusCurrentUser = "current_user"
	memberCandidateStatusAlready     = "already_member"
	memberCandidateStatusInvitable   = "invitable"
)

// projectMemberCandidateRow is the database projection used by the member
// picker. Status is classified by SQL so the count and page do not need any
// per-row lookups or client-side membership joins.
type projectMemberCandidateRow struct {
	UID      string `db:"uid"`
	Name     string `db:"candidate_name"`
	Status   string `db:"status"`
	Priority int    `db:"candidate_priority"`
}

type projectMemberCandidatesResult struct {
	Rows  []*projectMemberCandidateRow
	Total int64
}
