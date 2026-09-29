package user

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileContactInfoProjection(t *testing.T) {
	for _, tc := range []struct {
		name, phone, email, zone string
	}{{"populated", "13800001234", "alice@example.com", "0086"}, {"empty", "", "", ""}, {"international", "+442079460123", "alice@example.co.uk", "+44"}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			mock.ExpectQuery(`SELECT \* FROM user WHERE \(uid='peer'\)`).WillReturnRows(
				sqlmock.NewRows([]string{"uid", "status", "phone", "email", "zone"}).AddRow("peer", 1, tc.phone, tc.email, tc.zone))
			original := &UserDetailResp{UID: "peer", Name: "Alice", Remark: "Colleague"}
			resp, err := (&User{db: db}).profileWithContactInfo(original, true)
			require.NoError(t, err)
			body, err := json.Marshal(resp)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(body, &fields))
			assert.Equal(t, tc.phone, fields["phone"], "empty phone must still be present")
			assert.Equal(t, tc.email, fields["email"], "empty email must still be present")
			assert.Equal(t, "Colleague", fields["remark"])
			if tc.zone == "0086" {
				assert.Equal(t, "86", fields["phone_country_code"])
			} else if tc.zone == "+44" {
				assert.Equal(t, "44", fields["phone_country_code"])
			}
			shared, err := json.Marshal(original)
			require.NoError(t, err)
			assert.NotContains(t, string(shared), `"phone"`, "must not mutate the shared service DTO")
			assert.NotContains(t, string(shared), `"email"`)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestProfileContactInfoWithheld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		profile *UserDetailResp
	}{{"off", false, &UserDetailResp{UID: "peer"}}, {"bot", true, &UserDetailResp{UID: "bot", Robot: 1}},
		{"system bot", true, &UserDetailResp{UID: "fileHelper"}}, {"destroyed", true, &UserDetailResp{UID: "peer", IsDestroy: IsDestroyDone}}} {
		t.Run(tc.name, func(t *testing.T) {
			// No DB at all: these paths must not read contact data.
			resp, err := (&User{}).profileWithContactInfo(tc.profile, tc.enabled)
			require.NoError(t, err)
			assert.Same(t, tc.profile, resp)
		})
	}
	for _, tc := range []struct {
		name, state              string
		status, robot, destroyed int
	}{{"disabled", "", 0, 0, 0}, {"destroyed during read", "", 1, 0, 2}, {"bot during read", "", 1, 1, 0}, {"missing", "missing", 1, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			rows := sqlmock.NewRows([]string{"uid", "status", "robot", "is_destroy", "phone", "email"})
			if tc.state != "missing" {
				rows.AddRow("peer", tc.status, tc.robot, tc.destroyed, "13800001234", "hidden@example.com")
			}
			mock.ExpectQuery(`SELECT \* FROM user WHERE \(uid='peer'\)`).WillReturnRows(rows)
			profile := &UserDetailResp{UID: "peer"}
			resp, err := (&User{db: db}).profileWithContactInfo(profile, true)
			require.NoError(t, err)
			assert.Same(t, profile, resp)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	t.Run("query error", func(t *testing.T) {
		db, mock := newPhoneLookupMock(t)
		want := errors.New("database unavailable")
		mock.ExpectQuery(`SELECT \* FROM user WHERE \(uid='peer'\)`).WillReturnError(want)
		resp, err := (&User{db: db}).profileWithContactInfo(&UserDetailResp{UID: "peer"}, true)
		require.ErrorIs(t, err, want)
		assert.Nil(t, resp)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestProfileContactInfoHTTPVisibility(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	for _, uid := range []string{"contact_friend", "contact_space", "contact_group", "contact_stranger", "contact_empty"} {
		seedBatchUser(t, ctx, uid, uid, 1, 0)
	}
	_, err := ctx.DB().UpdateBySql("UPDATE user SET phone='', email='' WHERE uid='contact_empty'").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, 'contact_friend', 0)", testutil.UID).Exec()
	require.NoError(t, err)
	seedSpace(t, ctx, "contact_space", 1)
	for _, uid := range []string{testutil.UID, "contact_space", "contact_empty"} {
		seedSpaceMemberRow(t, ctx, "contact_space", uid)
	}
	_, err = ctx.DB().InsertBySql("INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('contact_group', 'Contacts', ?, 1, 1)", testutil.UID).Exec()
	require.NoError(t, err)
	for _, uid := range []string{testutil.UID, "contact_group"} {
		seedUserGroupMember(t, ctx, "contact_group", uid)
	}

	for _, enabled := range []string{"0", "1", "0"} {
		setSystemSettingForUserTest(t, ctx, "profile", "contact_info_on", enabled, "bool")
		require.NoError(t, settings.Reload())
		for _, uid := range []string{testutil.UID, "contact_friend", "contact_space", "contact_group", "contact_stranger", "contact_empty"} {
			t.Run(enabled+"/"+uid, func(t *testing.T) {
				w := getUser(s, uid)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var body map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				if enabled == "1" && uid != "contact_stranger" {
					if uid == testutil.UID || uid == "contact_empty" {
						assert.Equal(t, "", body["phone"])
						assert.Equal(t, "", body["email"])
					} else {
						assert.Equal(t, "13800008888", body["phone"])
						assert.Equal(t, uid+"@example.com", body["email"])
					}
				} else {
					assert.NotContains(t, body, "phone")
					assert.NotContains(t, body, "email")
				}
			})
		}
	}
	setSystemSettingForUserTest(t, ctx, "profile", "contact_info_on", "1", "bool")
	require.NoError(t, settings.Reload())
	shared, err := NewService(ctx).GetUserDetail("contact_friend", testutil.UID)
	require.NoError(t, err)
	assert.Empty(t, shared.Phone)
	assert.Empty(t, shared.Email)
	batch := decodeBatchResp(t, postBatchUsers(s, testutil.Token, `{"uids":["contact_friend"]}`))
	require.Len(t, batch.Users, 1)
	assert.NotContains(t, batch.Users[0], "phone")
	assert.NotContains(t, batch.Users[0], "email")
}
