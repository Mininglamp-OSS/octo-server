package bot_api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type fakeGroupServiceForRename struct {
	existMemberActive bool
	existMemberErr    error
}

func (f *fakeGroupServiceForRename) GetAllGroupCount() (int64, error) { return 0, nil }
func (f *fakeGroupServiceForRename) GetCreatedCountWithDate(date string) (int64, error) {
	return 0, nil
}
func (f *fakeGroupServiceForRename) AddGroup(model *group.AddGroupReq) error { return nil }
func (f *fakeGroupServiceForRename) GetGroupWithDateSpace(startDate, endDate string) (map[string]int64, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetGroupWithGroupNo(groupNo string) (*group.InfoResp, error) { return nil, nil }
func (f *fakeGroupServiceForRename) GetGroups(groupNos []string) ([]*group.InfoResp, error)    { return nil, nil }
func (f *fakeGroupServiceForRename) GetGroupDetails(groupNos []string, uid string) ([]*group.GroupResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetGroupDetail(groupNo, uid string) (*group.GroupResp, error) { return nil, nil }
func (f *fakeGroupServiceForRename) GetSettings(groupNos []string, uid string) ([]*group.SettingResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetSettingsWithUIDs(groupNo string, uids []string) ([]*group.SettingResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetMembers(groupNo string) ([]*group.MemberResp, error) { return nil, nil }
func (f *fakeGroupServiceForRename) GetMemberExternalMarkers(groupNo string) (map[string]group.MemberExternalMarker, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetMemberExternalFields(groupNo, uid string) (int, string, string, string, string, error) {
	return 0, "", "", "", "", nil
}
func (f *fakeGroupServiceForRename) GetMember(groupNo, uid string) (*group.MemberResp, error) { return nil, nil }
func (f *fakeGroupServiceForRename) GetBlacklistMemberUIDs(groupNo string) ([]string, error) { return nil, nil }
func (f *fakeGroupServiceForRename) GetSubscribableMemberUIDs(groupNo string) ([]string, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetMemberUIDsOfManager(groupNo string) ([]string, error) { return nil, nil }
func (f *fakeGroupServiceForRename) IsCreatorOrManager(groupNo, uid string) (bool, error)    { return false, nil }
func (f *fakeGroupServiceForRename) IsRobot(uid string) (bool, error)                        { return false, nil }
func (f *fakeGroupServiceForRename) GetMemberTotalAndOnlineCount(groupNo string) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeGroupServiceForRename) ExistMember(groupNo, uid string) (bool, error) {
	return f.existMemberActive, f.existMemberErr
}
func (f *fakeGroupServiceForRename) ExistMemberActive(groupNo, uid string) (bool, error) {
	return f.existMemberActive, f.existMemberErr
}
func (f *fakeGroupServiceForRename) ExistMemberActiveInternal(groupNo, uid string) (bool, error) { return false, nil }
func (f *fakeGroupServiceForRename) ExistMembers(groupNos []string, uid string) ([]string, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) ExistMembersActive(groupNos []string, uid string) ([]string, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetGroupsWithMemberUID(uid string) ([]*group.InfoResp, error) { return nil, nil }
func (f *fakeGroupServiceForRename) GetGroupMemberMaxVersion(groupNo string) (int64, error)      { return 0, nil }
func (f *fakeGroupServiceForRename) GetUserSupers(uid string) ([]*group.InfoResp, error)        { return nil, nil }
func (f *fakeGroupServiceForRename) AddMember(model *group.AddMemberReq) error                  { return nil }
func (f *fakeGroupServiceForRename) GetMembersWithUIDAndGroupIds(uid string, groupNos []string) ([]*group.MemberResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetManagersWithGroupNos(groupNos []string) ([]*group.MemberResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) GetGroupMd(groupNo string) (*group.GroupMdResult, error) { return nil, nil }
func (f *fakeGroupServiceForRename) UpdateGroupMd(groupNo, content, updatedBy string) (int64, error) {
	return 0, nil
}
func (f *fakeGroupServiceForRename) DeleteGroupMd(groupNo string) (int64, error) { return 0, nil }
func (f *fakeGroupServiceForRename) IsBotAdmin(groupNo, uid string) (bool, error) {
	return false, nil
}
func (f *fakeGroupServiceForRename) GetBotMemberUIDs(groupNo string) ([]string, error) { return nil, nil }
func (f *fakeGroupServiceForRename) CreateGroup(req *group.CreateGroupServiceReq) (*group.CreateGroupServiceResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) AddGroupMembers(req *group.AddGroupMembersServiceReq) (*group.AddGroupMembersServiceResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) RemoveGroupMembers(req *group.RemoveGroupMembersServiceReq) (*group.RemoveGroupMembersServiceResp, error) {
	return nil, nil
}
func (f *fakeGroupServiceForRename) RemoveUserFromGroupThreads(groupNo, uid, spaceID string) {}
func (f *fakeGroupServiceForRename) UpdateGroupInfo(req *group.UpdateGroupInfoServiceReq) error {
	return nil
}
func (f *fakeGroupServiceForRename) UpdateGroupAvatarCustom(req *group.UpdateGroupAvatarCustomServiceReq) error {
	return nil
}

type fakeThreadServiceForRename struct {
	updateNameAsBotErr error
	calledGroupNo      string
	calledShortID      string
	calledName         string
}

func (f *fakeThreadServiceForRename) UpdateName(groupNo, shortID, operatorUID, name string) error {
	return nil
}
func (f *fakeThreadServiceForRename) UpdateNameAsBot(groupNo, shortID, name string) error {
	f.calledGroupNo = groupNo
	f.calledShortID = shortID
	f.calledName = name
	return f.updateNameAsBotErr
}
func (f *fakeThreadServiceForRename) CreateThread(req *thread.CreateThreadReq) (*thread.ThreadResp, error) {
	return nil, nil
}
func (f *fakeThreadServiceForRename) GetThreads(groupNo string, statuses []int, pageIndex, pageSize int64) ([]*thread.ThreadResp, int64, error) {
	return nil, 0, nil
}
func (f *fakeThreadServiceForRename) GetThread(groupNo, shortID, loginUID string) (*thread.ThreadResp, error) {
	return nil, nil
}
func (f *fakeThreadServiceForRename) ArchiveThread(groupNo, shortID, operatorUID string) error   { return nil }
func (f *fakeThreadServiceForRename) UnarchiveThread(groupNo, shortID, operatorUID string) error { return nil }
func (f *fakeThreadServiceForRename) DeleteThread(groupNo, shortID, operatorUID string) error   { return nil }
func (f *fakeThreadServiceForRename) CanDelete(groupNo, shortID, uid string) (bool, error)      { return false, nil }
func (f *fakeThreadServiceForRename) ExistThread(groupNo, shortID string) (bool, error)         { return false, nil }
func (f *fakeThreadServiceForRename) JoinThread(groupNo, shortID, uid string) error             { return nil }
func (f *fakeThreadServiceForRename) LeaveThread(groupNo, shortID, uid string) error            { return nil }
func (f *fakeThreadServiceForRename) GetMembers(groupNo, shortID string) ([]*thread.MemberResp, error) {
	return nil, nil
}
func (f *fakeThreadServiceForRename) GetMemberUIDs(groupNo, shortID string) ([]string, error) { return nil, nil }
func (f *fakeThreadServiceForRename) IsMember(groupNo, shortID, uid string) (bool, error)     { return false, nil }
func (f *fakeThreadServiceForRename) GetThreadMd(groupNo, shortID string) (*thread.ThreadMdResult, error) {
	return nil, nil
}
func (f *fakeThreadServiceForRename) UpdateThreadMd(groupNo, shortID, content, updatedBy string) (int64, error) {
	return 0, nil
}
func (f *fakeThreadServiceForRename) DeleteThreadMd(groupNo, shortID, deletedBy string) (int64, error) {
	return 0, nil
}
func (f *fakeThreadServiceForRename) CanEditThreadMd(groupNo, shortID, uid string) (bool, error) {
	return false, nil
}
func (f *fakeThreadServiceForRename) UpdateSetting(groupNo, shortID, uid string, settings map[string]interface{}) error {
	return nil
}
func (f *fakeThreadServiceForRename) GetSettingsWithUIDs(groupNo, shortID string, uids []string) ([]*thread.SettingResp, error) {
	return nil, nil
}

const (
	trBotToken = "bf_rename_test_token"
	trRobotID  = "bot_rename_test"
	// valid 32-hex groupNo
	trGroupNo = "0123456789abcdef0123456789abcdef"
	// valid 15-20 digit shortID
	trShortID = "148910429168271360"
)

func newRenameTestEngine(t *testing.T, groupSvc *fakeGroupServiceForRename, threadSvc *fakeThreadServiceForRename) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-rename-test"),
		groupService:  groupSvc,
		threadService: threadSvc,
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			c.Set(CtxKeyRobotID, trRobotID)
			c.Set(CtxKeyBotKind, BotKindUser)
		}
		c.Next()
	})
	r.PUT("/v1/bot/groups/:group_no/threads/:short_id", func(gc *gin.Context) {
		ba.botRenameThread(&wkhttp.Context{Context: gc})
	})
	return r
}

func doRenamePUT(t *testing.T, r *gin.Engine, groupNo, shortID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewBuffer(b)
	} else {
		buf = &bytes.Buffer{}
	}
	path := "/v1/bot/groups/" + groupNo + "/threads/" + shortID
	req, err := http.NewRequest(http.MethodPut, path, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+trBotToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Happy path: valid params, service returns nil → 200 OK
func TestBotRenameThread_HappyPath(t *testing.T) {
	groupSvc := &fakeGroupServiceForRename{existMemberActive: true}
	threadSvc := &fakeThreadServiceForRename{}
	r := newRenameTestEngine(t, groupSvc, threadSvc)

	w := doRenamePUT(t, r, trGroupNo, trShortID, map[string]string{"name": "new thread name"})
	assert.Equal(t, http.StatusOK, w.Code, "happy path should return 200, body=%s", w.Body.String())
	assert.Equal(t, trGroupNo, threadSvc.calledGroupNo)
	assert.Equal(t, trShortID, threadSvc.calledShortID)
	assert.Equal(t, "new thread name", threadSvc.calledName)
}

// Error path: empty/missing name → 400 (bind error)
func TestBotRenameThread_EmptyName(t *testing.T) {
	groupSvc := &fakeGroupServiceForRename{existMemberActive: true}
	threadSvc := &fakeThreadServiceForRename{}
	r := newRenameTestEngine(t, groupSvc, threadSvc)

	w := doRenamePUT(t, r, trGroupNo, trShortID, map[string]string{"name": ""})
	assert.Equal(t, http.StatusBadRequest, w.Code, "empty name should return 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIRequestInvalid.DefaultMessage)
}

// Error path: invalid short_id (non-numeric) → 400
func TestBotRenameThread_InvalidShortID(t *testing.T) {
	groupSvc := &fakeGroupServiceForRename{existMemberActive: true}
	threadSvc := &fakeThreadServiceForRename{}
	r := newRenameTestEngine(t, groupSvc, threadSvc)

	w := doRenamePUT(t, r, trGroupNo, "abc123", map[string]string{"name": "new name"})
	assert.Equal(t, http.StatusBadRequest, w.Code, "invalid short_id should return 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIRequestInvalid.DefaultMessage)
}

// Error path: invalid group_no (wrong length) → 400
func TestBotRenameThread_InvalidGroupNo(t *testing.T) {
	groupSvc := &fakeGroupServiceForRename{existMemberActive: true}
	threadSvc := &fakeThreadServiceForRename{}
	r := newRenameTestEngine(t, groupSvc, threadSvc)

	w := doRenamePUT(t, r, "shortgno", trShortID, map[string]string{"name": "new name"})
	assert.Equal(t, http.StatusBadRequest, w.Code, "invalid group_no should return 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIRequestInvalid.DefaultMessage)
}

// Error path: service UpdateNameAsBot returns error → 500 (ErrBotAPIStoreFailed)
func TestBotRenameThread_ServiceError(t *testing.T) {
	groupSvc := &fakeGroupServiceForRename{existMemberActive: true}
	threadSvc := &fakeThreadServiceForRename{updateNameAsBotErr: errors.New("db error")}
	r := newRenameTestEngine(t, groupSvc, threadSvc)

	w := doRenamePUT(t, r, trGroupNo, trShortID, map[string]string{"name": "new name"})
	// ErrBotAPIStoreFailed via ResponseErrorL returns legacy 400 (D14 compatibility)
	assert.Equal(t, http.StatusBadRequest, w.Code, "service error should return 400 (D14 legacy), body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIStoreFailed.DefaultMessage)
}
