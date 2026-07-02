package group

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/require"
)

// TestGroupAvatarUpload_ManagerAllowed_CommonDenied covers the #520 relaxation:
// the group owner AND an admin can upload the group avatar, while a plain
// member is still rejected with creator_or_manager_only. Uses the mock file
// service (no real object storage); the permission gate runs before the upload.
func TestGroupAvatarUpload_ManagerAllowed_CommonDenied(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	f := New(ctx)
	mockFS := &mockAvatarUploadFileService{}
	f.fileService = mockFS

	const (
		groupNo      = "g-avatar-perm-520"
		managerUID   = "20000"
		managerToken = "token-manager-20000"
		commonUID    = "30000"
		commonToken  = "token-common-30000"
	)

	// Register manager/common token→uid (creator reuses testutil.Token/UID).
	cfg := ctx.GetConfig()
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+managerToken, managerUID+"@test"))
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+commonToken, commonUID+"@test"))

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "owner", ShortNo: "u_owner_520"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: managerUID, Name: "manager", ShortNo: "u_mgr_520"}))

	// Group with three roles: creator / manager / common.
	require.NoError(t, f.db.Insert(&Model{GroupNo: groupNo, Name: "权限群", Creator: testutil.UID, Status: GroupStatusNormal, Version: 1}))
	require.NoError(t, f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Status: 1, Version: 1, Vercode: "c@1"}))
	require.NoError(t, f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: managerUID, Role: MemberRoleManager, Status: 1, Version: 1, Vercode: "m@1"}))
	require.NoError(t, f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: commonUID, Role: MemberRoleCommon, Status: 1, Version: 1, Vercode: "n@1"}))

	r := wkhttp.New()
	r.POST("/v1/groups/:group_no/avatar", ctx.AuthMiddleware(r), f.avatarUpload)

	upload := func(t *testing.T, token string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "avatar.png")
		require.NoError(t, err)
		_, err = fw.Write([]byte("png"))
		require.NoError(t, err)
		require.NoError(t, mw.Close())

		w := httptest.NewRecorder()
		req, err := http.NewRequest("POST", "/v1/groups/"+groupNo+"/avatar", &buf)
		require.NoError(t, err)
		req.Header.Set("token", token)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		r.ServeHTTP(w, req)
		return w
	}

	// Owner: still allowed (unchanged behavior).
	require.Equal(t, http.StatusOK, upload(t, testutil.Token).Code, "owner should upload")

	// Admin: now allowed (#520 relaxation, aligned with group name/settings).
	wMgr := upload(t, managerToken)
	require.Equal(t, http.StatusOK, wMgr.Code, "group admin should be allowed to upload, body=%s", wMgr.Body.String())
	require.Contains(t, wMgr.Body.String(), `"status":200`)

	// Plain member: still rejected. The test-env wire body carries only the
	// localized message + status (no error.code field), so assert on the message.
	// "owner or an administrator" is unique to ErrGroupCreatorOrManagerOnly and
	// distinguishes it from the old creator_only ("Only the group owner can…"),
	// proving the member is rejected by the new (post-relaxation) code.
	// Reset the mock first so the empty-uploadedPath assertion proves the denial
	// happens BEFORE storage (owner/manager successes above already set it).
	mockFS.uploadedPath = ""
	wCommon := upload(t, commonToken)
	require.Contains(t, wCommon.Body.String(), "owner or an administrator",
		"plain member should be rejected with creator_or_manager_only, body=%s", wCommon.Body.String())
	require.Contains(t, wCommon.Body.String(), `"status":400`, "denied member should get wire 400")
	require.Empty(t, mockFS.uploadedPath, "permission denial must occur before the upload reaches object storage")
}
