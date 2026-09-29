package user

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
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
			} else if tc.zone == "" {
				assert.NotContains(t, fields, "zone", "unset formatting metadata is optional")
				assert.NotContains(t, fields, "phone_country_code")
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
		{"blocked caller", true, &UserDetailResp{UID: "peer", BeBlacklist: 1}},
		{"system bot", true, &UserDetailResp{UID: "fileHelper"}},
		{"system category", true, &UserDetailResp{UID: "peer", Category: CategorySystem}},
		{"customer service category", true, &UserDetailResp{UID: "peer", Category: CategoryCustomerService}},
		{"destroyed", true, &UserDetailResp{UID: "peer", IsDestroy: IsDestroyDone}}} {
		t.Run(tc.name, func(t *testing.T) {
			// No DB at all: these paths must not read contact data.
			resp, err := (&User{}).profileWithContactInfo(tc.profile, tc.enabled)
			require.NoError(t, err)
			assert.Same(t, tc.profile, resp)
		})
	}
	for _, tc := range []struct {
		name, state, category, role string
		status, robot, destroyed    int
	}{
		{name: "disabled"},
		{name: "destroyed during read", status: 1, destroyed: 2},
		{name: "bot during read", status: 1, robot: 1},
		{name: "missing", state: "missing", status: 1},
		{name: "system category during read", category: CategorySystem, status: 1},
		{name: "customer service category during read", category: CategoryCustomerService, status: 1},
		{name: "admin role during read", role: string(wkhttp.Admin), status: 1},
		{name: "super admin role during read", role: string(wkhttp.SuperAdmin), status: 1},
		{name: "dashboard role during read", role: auth.ManagerRoleDashboardReader, status: 1},
		{name: "market role during read", role: auth.ManagerRoleMarketAdmin, status: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			rows := sqlmock.NewRows([]string{"uid", "status", "robot", "is_destroy", "phone", "email", "category", "role"})
			if tc.state != "missing" {
				rows.AddRow("peer", tc.status, tc.robot, tc.destroyed, "13800001234", "hidden@example.com", tc.category, tc.role)
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

func TestProfileContactInfoHTTPPrivilegedAccounts(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	setSystemSettingForUserTest(t, ctx, "profile", "contact_info_on", "1", "bool")
	require.NoError(t, settings.Reload())
	seedSpace(t, ctx, "privileged_space", 1)
	seedSpaceMemberRow(t, ctx, "privileged_space", testutil.UID)
	_, err := ctx.DB().InsertBySql("INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('privileged_group', 'Privileged', ?, 1, 1)", testutil.UID).Exec()
	require.NoError(t, err)
	seedUserGroupMember(t, ctx, "privileged_group", testutil.UID)

	for i, tc := range []struct{ name, category, role string }{
		{"system category", CategorySystem, ""},
		{"seeded super admin", CategorySystem, string(wkhttp.SuperAdmin)},
		{"customer service", CategoryCustomerService, ""},
		{"admin", "", string(wkhttp.Admin)},
		{"super admin", "", string(wkhttp.SuperAdmin)},
		{"dashboard reader", "", auth.ManagerRoleDashboardReader},
		{"market admin", "", auth.ManagerRoleMarketAdmin},
	} {
		for j, relation := range []string{"friend", "space", "group"} {
			t.Run(tc.name+"/"+relation, func(t *testing.T) {
				uid := fmt.Sprintf("contact_priv_%d_%d", i, j)
				seedBatchUser(t, ctx, uid, tc.name, 1, 0)
				_, err := ctx.DB().Update("user").Set("category", tc.category).Set("role", tc.role).Where("uid=?", uid).Exec()
				require.NoError(t, err)
				switch relation {
				case "friend":
					_, err := ctx.DB().InsertBySql("INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)", testutil.UID, uid).Exec()
					require.NoError(t, err)
				case "space":
					seedSpaceMemberRow(t, ctx, "privileged_space", uid)
				case "group":
					seedUserGroupMember(t, ctx, "privileged_group", uid)
				}
				w := getUser(s, uid)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var body map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				require.Contains(t, body, "category", "must reach the full-profile path, not pass via stranger redaction")
				assert.Equal(t, tc.category, body["category"])
				for _, key := range []string{"phone", "email", "zone", "phone_country_code"} {
					assert.NotContains(t, body, key)
				}
			})
		}
	}

	// The new restriction concerns peer disclosure; the historical self-only
	// contact fields must remain available to their owner, even for a manager.
	_, err = ctx.DB().Update("user").Set("category", CategorySystem).
		Set("role", string(wkhttp.SuperAdmin)).Set("phone", "13800001234").
		Set("email", "self-manager@example.com").Where("uid=?", testutil.UID).Exec()
	require.NoError(t, err)
	w := getUser(s, testutil.UID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var self map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &self))
	assert.Equal(t, "13800001234", self["phone"])
	assert.Equal(t, "self-manager@example.com", self["email"])
}

func TestProfileContactInfoHTTPVisibility(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	for _, uid := range []string{"contact_friend", "contact_blocker", "contact_space", "contact_group", "contact_stranger", "contact_empty"} {
		seedBatchUser(t, ctx, uid, uid, 1, 0)
	}
	_, err := ctx.DB().UpdateBySql("UPDATE user SET phone='', email='' WHERE uid='contact_empty'").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, 'contact_friend', 0)", testutil.UID).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, 'contact_blocker', 0)", testutil.UID).Exec()
	require.NoError(t, err)
	// The target has blocked the caller. The friend row deliberately remains
	// active, proving the contact projection enforces the relationship state
	// instead of relying on the existing full-profile visibility decision.
	_, err = ctx.DB().InsertBySql("INSERT INTO user_setting (uid, to_uid, blacklist) VALUES ('contact_blocker', ?, 1)", testutil.UID).Exec()
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
		for _, uid := range []string{testutil.UID, "contact_friend", "contact_blocker", "contact_space", "contact_group", "contact_stranger", "contact_empty"} {
			t.Run(enabled+"/"+uid, func(t *testing.T) {
				w := getUser(s, uid)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var body map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				if enabled == "1" && (uid == testutil.UID || uid == "contact_friend" || uid == "contact_space" || uid == "contact_empty") {
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

// Only friendship or active shared Space membership grants peer contacts.
// Keep group rows throughout the lifecycle: they must not override revocation.
func TestProfileContactInfoHTTPRequiresContactRelation(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	setSystemSettingForUserTest(t, ctx, "profile", "contact_info_on", "1", "bool")
	require.NoError(t, settings.Reload())
	for _, relation := range []string{"friend", "group", "friend_and_group"} {
		t.Run(relation, func(t *testing.T) {
			uid, spaceID, groupNo := "contact_"+relation, "space_"+relation, "group_"+relation
			seedBatchUser(t, ctx, uid, uid, 1, 0)
			seedSpace(t, ctx, spaceID, 1)
			seedSpaceMemberRow(t, ctx, spaceID, testutil.UID)
			if relation != "group" {
				_, err := ctx.DB().InsertBySql("INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)", testutil.UID, uid).Exec()
				require.NoError(t, err)
			}
			_, err := ctx.DB().InsertBySql("INSERT INTO `group` (group_no, name, creator, space_id, status, version) VALUES (?, 'Contacts', ?, ?, 1, 1)", groupNo, testutil.UID, spaceID).Exec()
			require.NoError(t, err)
			if relation != "friend" {
				seedUserGroupMember(t, ctx, groupNo, testutil.UID)
				seedUserGroupMember(t, ctx, groupNo, uid)
			}
			hasFriend := relation != "group"
			check := func(wantContact bool) {
				t.Helper()
				resetUserUIDRateLimit(t, ctx)
				w := getUser(s, uid)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var body map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				require.Contains(t, body, "category", "basic profile remains visible")
				if wantContact || hasFriend {
					assert.Equal(t, "13800008888", body["phone"])
					assert.Equal(t, uid+"@example.com", body["email"])
				} else {
					for _, field := range []string{"phone", "email", "zone", "phone_country_code"} {
						assert.NotContains(t, body, field)
					}
				}
			}
			t.Run("outside space", func(t *testing.T) { check(false) })
			seedSpaceMemberRow(t, ctx, spaceID, uid)
			t.Run("joined space", func(t *testing.T) { check(true) })
			for _, status := range []int{2, 0, 1} {
				t.Run(fmt.Sprintf("space status %d", status), func(t *testing.T) {
					_, err := ctx.DB().Update("space").Set("status", status).Where("space_id=?", spaceID).Exec()
					require.NoError(t, err)
					check(status == 1)
				})
			}
			for _, member := range []string{uid, testutil.UID} {
				for _, status := range []int{0, 1} {
					t.Run(fmt.Sprintf("member %s status %d", member, status), func(t *testing.T) {
						_, err := ctx.DB().Update("space_member").Set("status", status).Where("space_id=? AND uid=?", spaceID, member).Exec()
						require.NoError(t, err)
						check(status == 1)
					})
				}
			}
			_, err = ctx.DB().Update("group").Set("status", 0).Where("group_no=?", groupNo).Exec()
			require.NoError(t, err)
			t.Run("disabled group with active space", func(t *testing.T) { check(true) })
			_, err = ctx.DB().Update("space").Set("status", 2).Where("space_id=?", spaceID).Exec()
			require.NoError(t, err)
			t.Run("disabled group with banned space", func(t *testing.T) { check(false) })
			if relation == "friend_and_group" {
				for _, deleted := range []int{1, 0} {
					t.Run(fmt.Sprintf("friend deleted %d", deleted), func(t *testing.T) {
						_, err := ctx.DB().Update("friend").Set("is_deleted", deleted).Where("uid=? AND to_uid=?", testutil.UID, uid).Exec()
						require.NoError(t, err)
						hasFriend = deleted == 0
						check(false)
					})
				}
			}
		})
	}
}

func TestProfileContactInfoHTTPBlockUnblock(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	setSystemSettingForUserTest(t, ctx, "profile", "contact_info_on", "1", "bool")
	require.NoError(t, settings.Reload())
	for _, relation := range []string{"friend", "space"} {
		t.Run(relation, func(t *testing.T) {
			uid := "contact_block_" + relation
			seedBatchUser(t, ctx, uid, uid, 1, 0)
			if relation == "friend" {
				_, err := ctx.DB().InsertBySql("INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)", testutil.UID, uid).Exec()
				require.NoError(t, err)
			} else {
				seedSpace(t, ctx, "contact_block_space", 1)
				seedSpaceMemberRow(t, ctx, "contact_block_space", testutil.UID)
				seedSpaceMemberRow(t, ctx, "contact_block_space", uid)
			}
			_, err := ctx.DB().InsertBySql("INSERT INTO user_setting (uid, to_uid, blacklist) VALUES (?, ?, 0)", uid, testutil.UID).Exec()
			require.NoError(t, err)
			for _, blocked := range []int{0, 1, 0} {
				t.Run(fmt.Sprintf("blocked %d", blocked), func(t *testing.T) {
					_, err := ctx.DB().Update("user_setting").Set("blacklist", blocked).Where("uid=? AND to_uid=?", uid, testutil.UID).Exec()
					require.NoError(t, err)
					w := getUser(s, uid)
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					var body map[string]any
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
					require.Contains(t, body, "category")
					if blocked == 0 {
						assert.Equal(t, "13800008888", body["phone"])
						assert.Equal(t, uid+"@example.com", body["email"])
					} else {
						for _, field := range []string{"phone", "email", "zone", "phone_country_code"} {
							assert.NotContains(t, body, field)
						}
					}
				})
			}
		})
	}
}

func TestProfileContactInfoHTTPQueryFailureKeepsBasicProfile(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	setSystemSettingForUserTest(t, ctx, "profile", "contact_info_on", "1", "bool")
	require.NoError(t, settings.Reload())
	seedBatchUser(t, ctx, "contact_query_failure", "Contact peer", 1, 0)
	seedSpace(t, ctx, "contact_query_space", 1)
	seedSpaceMemberRow(t, ctx, "contact_query_space", testutil.UID)
	seedSpaceMemberRow(t, ctx, "contact_query_space", "contact_query_failure")
	// The base profile and authorization use the real service/database. Inject
	// a failure only into the extra account lookup performed by the handler.
	u := New(ctx)
	db, mock := newPhoneLookupMock(t)
	u.db = db
	mock.ExpectQuery(`SELECT \* FROM user WHERE \(uid='contact_query_failure'\)`).WillReturnError(errors.New("contact query unavailable"))
	s.GetRoute().Group("/contact-test", ctx.AuthMiddleware(s.GetRoute())).GET("/:uid", u.get)
	req := httptest.NewRequest(http.MethodGet, "/contact-test/contact_query_failure", nil)
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "Contact peer", body["name"])
	assert.Contains(t, body, "category")
	for _, field := range []string{"phone", "email", "zone", "phone_country_code"} {
		assert.NotContains(t, body, field)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}
