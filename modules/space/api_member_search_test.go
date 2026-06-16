package space

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
)

type spaceMembersSearchResponse struct {
	Count int64                           `json:"count"`
	List  []spaceMembersSearchItemForTest `json:"list"`
}

type spaceMembersSearchItemForTest struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Role      int    `json:"role"`
	Robot     int    `json:"robot"`
	CreatedAt string `json:"created_at"`
}

func resetSpaceUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rdsClient := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rdsClient.Close()
	keys, err := rdsClient.Keys("ratelimit:uid:*").Result()
	if err == nil && len(keys) > 0 {
		_ = rdsClient.Del(keys...).Err()
	}
}

func getMembersSearch(t *testing.T, srv *server.Server, ctx *config.Context, spaceId string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	resetSpaceUIDRateLimit(t, ctx)
	path := "/v1/space/" + spaceId + "/members/search"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("token", testutil.Token)
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func decodeMembersSearchResp(t *testing.T, w *httptest.ResponseRecorder) spaceMembersSearchResponse {
	t.Helper()
	var resp spaceMembersSearchResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

func seedMemberSearchSpace(t *testing.T, spaceId, creator string) {
	t.Helper()
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    spaceId,
		Creator: creator,
		Status:  SpaceStatusNormal,
	}))
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     creator,
		Role:    2,
		Status:  1,
	}))
}

func seedMemberSearchMember(t *testing.T, spaceId, uid string, role, status int) {
	t.Helper()
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     uid,
		Role:    role,
		Status:  status,
	}))
}

func seedMemberSearchUser(t *testing.T, uid, name, username, email, phone string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, username, email, phone) VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE name=VALUES(name), username=VALUES(username), email=VALUES(email), phone=VALUES(phone)",
		uid, name, username, email, phone,
	).Exec()
	assert.NoError(t, err)
}

func seedMemberSearchRobot(t *testing.T, uid, name string) {
	t.Helper()
	seedMemberSearchUser(t, uid, name, uid, "", "")
	_, err := testCtx.DB().InsertInto("robot").Columns("robot_id", "token", "status", "creator_uid").
		Values(uid, "token-"+uid, 1, testutil.UID).Exec()
	assert.NoError(t, err)
}

func findMemberSearchItem(list []spaceMembersSearchItemForTest, uid string) (spaceMembersSearchItemForTest, bool) {
	for _, it := range list {
		if it.UID == uid {
			return it, true
		}
	}
	return spaceMembersSearchItemForTest{}, false
}

func TestMaskPhone(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"mainland mobile", "13812345678", "138****5678"},
		{"seven chars boundary keeps first three and last four", "1234567", "123****4567"},
		{"six chars fully masked", "123456", "***"},
		{"short value fully masked", "12345", "***"},
		{"empty value stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, maskPhone(tc.in))
		})
	}
}

func TestSpaceMembersSearchAuthz(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	ctx := testCtx

	t.Run("non member gets not_member", func(t *testing.T) {
		spaceId := "sp-search-nonmember"
		seedMemberSearchSpace(t, spaceId, "owner-nonmember")

		w := getMembersSearch(t, srv, ctx, spaceId, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assertSpaceErrorCode(t, w, "err.server.space.not_member")
	})

	t.Run("regular member gets permission_denied", func(t *testing.T) {
		assert.NoError(t, testutil.CleanAllTables(testCtx))
		spaceId := "sp-search-member"
		seedMemberSearchSpace(t, spaceId, "owner-member")
		seedMemberSearchMember(t, spaceId, testutil.UID, 0, 1)

		w := getMembersSearch(t, srv, ctx, spaceId, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
	})
}

func TestSpaceMembersSearchEmptyKeywordReturnsActiveMembersAndBots(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	spaceId := "sp-search-all"
	seedMemberSearchSpace(t, spaceId, "owner-all")
	seedMemberSearchMember(t, spaceId, testutil.UID, 1, 1)
	seedMemberSearchUser(t, "human-all", "Human All", "human_all", "human@example.com", "13812345678")
	seedMemberSearchMember(t, spaceId, "human-all", 0, 1)
	seedMemberSearchRobot(t, "bot-all", "Bot All")
	seedMemberSearchMember(t, spaceId, "bot-all", 0, 1)
	seedMemberSearchUser(t, "removed-all", "Removed All", "removed_all", "removed@example.com", "13912345678")
	seedMemberSearchMember(t, spaceId, "removed-all", 0, 0)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{
		"page_index": {"1"},
		"page_size":  {"20"},
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)
	assert.EqualValues(t, 4, resp.Count)
	assert.Len(t, resp.List, 4)

	human, ok := findMemberSearchItem(resp.List, "human-all")
	if assert.True(t, ok) {
		assert.Equal(t, "human@example.com", human.Email)
		assert.Equal(t, "138****5678", human.Phone)
		assert.Equal(t, 0, human.Robot)
	}
	bot, ok := findMemberSearchItem(resp.List, "bot-all")
	if assert.True(t, ok) {
		assert.Equal(t, 1, bot.Robot)
	}
	_, ok = findMemberSearchItem(resp.List, "removed-all")
	assert.False(t, ok, "removed members must not be visible from the space-side search")
}

func TestSpaceMembersSearchEmptyContactFieldsStayEmpty(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	spaceId := "sp-search-empty-contact"
	seedMemberSearchSpace(t, spaceId, testutil.UID)
	seedMemberSearchUser(t, "empty-contact", "Empty Contact", "empty_contact", "", "")
	seedMemberSearchMember(t, spaceId, "empty-contact", 0, 1)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{
		"keyword": {"empty_contact"},
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)
	if assert.Len(t, resp.List, 1) {
		assert.Equal(t, "", resp.List[0].Email)
		assert.Equal(t, "", resp.List[0].Phone)
	}
}

// TestSpaceMembersSearchIncludesBotsFromOtherCreators pins the deliberate admin-view
// behavior: unlike the member-facing listMembers (which only surfaces bots created by
// the caller), this admin search returns ALL bots in the space regardless of creator.
func TestSpaceMembersSearchIncludesBotsFromOtherCreators(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	spaceId := "sp-search-otherbot"
	seedMemberSearchSpace(t, spaceId, testutil.UID)
	// a bot created by someone other than the caller
	seedMemberSearchUser(t, "bot-other", "Bot Other", "bot_other", "", "")
	_, err = testCtx.DB().InsertInto("robot").Columns("robot_id", "token", "status", "creator_uid").
		Values("bot-other", "token-bot-other", 1, "someone-else").Exec()
	assert.NoError(t, err)
	seedMemberSearchMember(t, spaceId, "bot-other", 0, 1)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"keyword": {"bot_other"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)
	bot, ok := findMemberSearchItem(resp.List, "bot-other")
	if assert.True(t, ok, "admin search must surface bots created by others") {
		assert.Equal(t, 1, bot.Robot)
	}
}

func TestSpaceMembersSearchKeywordColumnsMaskingAndEscaping(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	spaceId := "sp-search-columns"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	fixtures := []struct {
		uid      string
		name     string
		username string
		email    string
		phone    string
	}{
		{"u-name-target", "Alice Cooper", "alice123", "alice@example.com", "13800001111"},
		{"u-username-target", "Bob", "zzqqxx", "bob.unique@corp.io", "13900002222"},
		{"u-phone-target", "Carol", "carol", "carol@example.com", "15512348888"},
		{"u-uid-target", "Dave", "dave", "dave@example.com", "15600003333"},
	}
	for _, f := range fixtures {
		seedMemberSearchUser(t, f.uid, f.name, f.username, f.email, f.phone)
		seedMemberSearchMember(t, spaceId, f.uid, 0, 1)
	}

	doSearch := func(keyword string) spaceMembersSearchResponse {
		w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"keyword": {keyword}})
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return decodeMembersSearchResp(t, w)
	}

	cases := []struct {
		name    string
		keyword string
		wantUID string
	}{
		{"by name", "Alice", "u-name-target"},
		{"by username", "zzqqxx", "u-username-target"},
		{"by email", "bob.unique", "u-username-target"},
		{"by phone last4", "8888", "u-phone-target"},
		{"by uid", "u-uid-target", "u-uid-target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doSearch(tc.keyword)
			assert.EqualValues(t, 1, resp.Count)
			if assert.Len(t, resp.List, 1) {
				assert.Equal(t, tc.wantUID, resp.List[0].UID)
			}
		})
	}

	t.Run("email returned in cleartext, phone masked to last 4", func(t *testing.T) {
		resp := doSearch("bob.unique")
		if assert.Len(t, resp.List, 1) {
			assert.Equal(t, "bob.unique@corp.io", resp.List[0].Email)
			assert.Equal(t, "139****2222", resp.List[0].Phone)
		}
	})

	t.Run("list and count share the same keyword filter", func(t *testing.T) {
		resp := doSearch("example.com")
		assert.EqualValues(t, 3, resp.Count)
		assert.Len(t, resp.List, 3)
	})

	t.Run("like wildcard characters are escaped", func(t *testing.T) {
		resp := doSearch("zzqqx_")
		assert.EqualValues(t, 0, resp.Count)
		assert.Len(t, resp.List, 0)
	})
}

// TestSpaceMembersSearchRejectsInactiveSpace pins the lifecycle authz gate:
// banning a space (status=2) only updates the space row and leaves space_member
// rows at status=1, so without an explicit active-space check a former admin
// could still search members (incl. masked email/phone) of a banned/disbanded
// space. checkSpaceActive (strict status=1) must reject both.
func TestSpaceMembersSearchRejectsInactiveSpace(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	cases := []struct {
		name   string
		status int
	}{
		{"banned", 2},
		{"disbanded", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, testutil.CleanAllTables(testCtx))
			spaceId := "sp-inactive-" + tc.name
			assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
				SpaceId: spaceId,
				Name:    spaceId,
				Creator: testutil.UID,
				Status:  tc.status,
			}))
			// caller is a still-active owner row (role=2, status=1) — the exact
			// state a ban leaves behind; only the space-level gate can reject it.
			assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
				SpaceId: spaceId,
				UID:     testutil.UID,
				Role:    2,
				Status:  1,
			}))
			w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{})
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}

func TestSpaceMembersSearchPaginationClamp(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	spaceId := "sp-search-page"
	seedMemberSearchSpace(t, spaceId, testutil.UID)
	for i := 0; i < 204; i++ {
		uid := fmt.Sprintf("page-member-%03d", i)
		seedMemberSearchUser(t, uid, uid, uid, "", "")
		seedMemberSearchMember(t, spaceId, uid, 0, 1)
	}

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{
		"page_index": {"1"},
		"page_size":  {"999"},
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)
	assert.EqualValues(t, 205, resp.Count)
	assert.Len(t, resp.List, 200, "page_size must be clamped to the shared 200-row cap")

	w = getMembersSearch(t, srv, testCtx, spaceId, url.Values{
		"page_index": {"2"},
		"page_size":  {"2"},
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp = decodeMembersSearchResp(t, w)
	assert.EqualValues(t, 205, resp.Count)
	assert.Len(t, resp.List, 2)
}
