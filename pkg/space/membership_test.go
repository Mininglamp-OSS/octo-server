package space

import (
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckMembershipEmptyArgs(t *testing.T) {
	ok, err := CheckMembership(nil, "", "uid1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckMembership(nil, "space1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid")
	}
}

func TestActiveSpacesForMember(t *testing.T) {
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer rawDB.Close()
	session := (&dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}).NewSession(nil)
	query := "SELECT sm.space_id FROM space_member sm " +
		"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 " +
		"WHERE sm.uid = 'u1' AND sm.status = 1"

	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WillReturnRows(sqlmock.NewRows([]string{"space_id"}).AddRow("spaceA").AddRow("spaceB"))
	got, err := ActiveSpacesForMember(session, "u1")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"spaceA": true, "spaceB": true}, got)
	require.NoError(t, mock.ExpectationsWereMet())

	mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnError(errors.New("query failed"))
	got, err = ActiveSpacesForMember(session, "u1")
	assert.EqualError(t, err, "query failed")
	assert.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActiveSpacesForMemberEmptyUID(t *testing.T) {
	got, err := ActiveSpacesForMember(nil, "")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got)
}

func TestCheckBothMembersEmptyArgs(t *testing.T) {
	ok, err := CheckBothMembers(nil, "", "uid1", "uid2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckBothMembers(nil, "space1", "", "uid2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid1")
	}
}

func TestHaveCommonSpaceEmptyArgs(t *testing.T) {
	tests := []struct {
		name string
		uid1 string
		uid2 string
	}{
		{"both_empty", "", ""},
		{"uid1_empty", "", "u2"},
		{"uid2_empty", "u1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := HaveCommonSpace(nil, tt.uid1, tt.uid2)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Errorf("expected false for %s", tt.name)
			}
		})
	}
}

func TestCheckMembershipForCleanupEmptyArgs(t *testing.T) {
	ok, err := CheckMembershipForCleanup(nil, "", "uid1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckMembershipForCleanup(nil, "space1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid")
	}
}
