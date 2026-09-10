package project

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

type recordingQueryMatcher struct {
	queries *[]string
}

func (m recordingQueryMatcher) Match(_, actualSQL string) error {
	*m.queries = append(*m.queries, actualSQL)
	return nil
}

func TestListMyProjectGroupResponsesByProjectIDsUsesTwoQueries(t *testing.T) {
	var queries []string
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(recordingQueryMatcher{queries: &queries}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawDB.Close() })

	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	db := &DB{session: conn.NewSession(nil)}

	mock.ExpectQuery("project groups").WillReturnRows(sqlmock.NewRows([]string{
		"project_id", "group_no", "name", "is_named", "avatar_text", "avatar_color", "is_upload_avatar",
	}).
		AddRow("project-a", "group-a1", "A1", 1, "A1", nil, 0).
		AddRow("project-a", "group-a2", "A2", 1, "A2", nil, 0).
		AddRow("project-a", "group-a3", "A3", 1, "A3", nil, 0).
		AddRow("project-b", "group-b1", "B1", 1, "B1", nil, 0))
	mock.ExpectQuery("member counts").WillReturnRows(sqlmock.NewRows([]string{"group_no", "member_count"}).
		AddRow("group-a1", 2).
		AddRow("group-a2", 3).
		AddRow("group-b1", 4))

	got, err := db.listMyProjectGroupResponsesByProjectIDs(
		"space", "uid", []string{"project-a", "project-b", "project-empty"}, 2,
	)
	require.NoError(t, err)
	require.Len(t, got["project-a"], 2)
	require.Equal(t, "group-a1", got["project-a"][0].GroupNo)
	require.Equal(t, 2, got["project-a"][0].MemberCount)
	require.Equal(t, "group-a2", got["project-a"][1].GroupNo)
	require.Equal(t, 3, got["project-a"][1].MemberCount)
	require.Len(t, got["project-b"], 1)
	require.Equal(t, 4, got["project-b"][0].MemberCount)
	require.Empty(t, got["project-empty"])

	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, queries, 2, "the batch cost must not grow with the number of Projects")
	require.Contains(t, queries[0], "g.project_id IN")
	require.NotContains(t, queries[1], "group-a3", "member counts must cover only the capped response rows")
}
