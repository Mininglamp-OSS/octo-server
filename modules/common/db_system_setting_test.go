package common

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemSettingDB_UpsertAndList(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	db := newSystemSettingDB(ctx)

	// Insert path.
	require.NoError(t, db.upsert("register", "email_on", "1", "bool", "邮箱注册开关"))
	require.NoError(t, db.upsert("support", "email_smtp", "smtp.example.com:465", "string", "SMTP"))

	rows, err := db.listAll()
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	got := map[string]*systemSettingModel{}
	for _, m := range rows {
		got[m.Category+"."+m.KeyName] = m
	}

	assert.Equal(t, "1", got["register.email_on"].Value)
	assert.Equal(t, "bool", got["register.email_on"].ValueType)
	assert.Equal(t, "邮箱注册开关", got["register.email_on"].Description)
	assert.Equal(t, "smtp.example.com:465", got["support.email_smtp"].Value)

	// Update path (unique key collision).
	require.NoError(t, db.upsert("register", "email_on", "0", "bool", "邮箱注册开关"))
	rows, err = db.listAll()
	require.NoError(t, err)
	assert.Len(t, rows, 2, "upsert must not create a duplicate row")

	for _, m := range rows {
		if m.Category == "register" && m.KeyName == "email_on" {
			assert.Equal(t, "0", m.Value)
		}
	}
}

func TestSystemSettingDB_EmptyListing(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	db := newSystemSettingDB(ctx)
	rows, err := db.listAll()
	require.NoError(t, err)
	assert.Empty(t, rows)
}
