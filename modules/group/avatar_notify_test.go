package group

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/require"
)

type imCapture struct {
	mu       sync.Mutex
	messages []capturedIMMessage
	server   *httptest.Server
}

type capturedIMMessage struct {
	request config.MsgSendReq
	payload map[string]any
}

func startIMCapture(t *testing.T, ctx *config.Context) *imCapture {
	t.Helper()
	cap := &imCapture{}
	cap.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req config.MsgSendReq
		_ = json.Unmarshal(body, &req)
		var payload map[string]any
		_ = json.Unmarshal(req.Payload, &payload)
		cap.mu.Lock()
		cap.messages = append(cap.messages, capturedIMMessage{request: req, payload: payload})
		cap.mu.Unlock()
		_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"ok"}}`))
	}))
	t.Cleanup(cap.server.Close)
	ctx.GetConfig().WuKongIM.APIURL = cap.server.URL
	return cap
}

func (c *imCapture) findAvatarChanged() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, message := range c.messages {
		content, _ := message.payload["content"].(string)
		if content == "{0}修改了群头像" {
			return message.payload
		}
	}
	return nil
}

func (c *imCapture) findAvatarChangedRequest() *config.MsgSendReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, message := range c.messages {
		content, _ := message.payload["content"].(string)
		if content == "{0}修改了群头像" {
			return &message.request
		}
	}
	return nil
}

func (c *imCapture) countAvatarChanged() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, message := range c.messages {
		content, _ := message.payload["content"].(string)
		if content == "{0}修改了群头像" {
			n++
		}
	}
	return n
}

func newAvatarNotifyContext(t *testing.T) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.EventPoolSize = 1
	cfg.Push.PushPoolSize = 1
	return config.NewContext(cfg)
}

func TestSendGroupAvatarChangedMessagePayload(t *testing.T) {
	ctx := newAvatarNotifyContext(t)
	cap := startIMCapture(t, ctx)

	require.NoError(t, sendGroupAvatarChangedMessage(ctx, "g_avatar_tip", "u_op", "Alice"))
	payload := cap.findAvatarChanged()
	require.NotNil(t, payload, "should send GroupUpdate system message")
	require.Equal(t, "{0}修改了群头像", payload["content"])
	require.Equal(t, float64(common.GroupUpdate), payload["type"])
	extra, ok := payload["extra"].([]any)
	require.True(t, ok)
	require.Len(t, extra, 1)
	op, ok := extra[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "u_op", op["uid"])
	require.Equal(t, "Alice", op["name"])
	request := cap.findAvatarChangedRequest()
	require.NotNil(t, request)
	require.Equal(t, "g_avatar_tip", request.ChannelID)
	require.Equal(t, common.ChannelTypeGroup.Uint8(), request.ChannelType)
	require.Equal(t, 1, request.Header.RedDot)
}

func TestSendGroupAvatarChangedMessageNoop(t *testing.T) {
	ctx := newAvatarNotifyContext(t)
	cap := startIMCapture(t, ctx)
	require.NoError(t, sendGroupAvatarChangedMessage(nil, "g_avatar_tip", "u_op", "Alice"))
	require.NoError(t, sendGroupAvatarChangedMessage(ctx, "", "u_op", "Alice"))
	require.Equal(t, 0, cap.countAvatarChanged())
}

func TestGroupUpdateAvatarCustomSendsSystemMessage(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)
	cap := startIMCapture(t, ctx)
	r := newAvatarNotifyGroupUpdateRouter(t, ctx, g)

	const groupNo = "avatar_tip_custom"
	seedCreatorGroup(t, g, groupNo, "原始群名")

	w := putGroupUpdate(t, r, groupNo, map[string]string{
		attrKeyAvatarText:  "研发",
		attrKeyAvatarColor: "5",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	payload := cap.findAvatarChanged()
	require.NotNil(t, payload, "custom avatar save should emit 修改了群头像")
	require.Equal(t, float64(common.GroupUpdate), payload["type"])
	request := cap.findAvatarChangedRequest()
	require.NotNil(t, request)
	require.Equal(t, groupNo, request.ChannelID)
	require.Equal(t, common.ChannelTypeGroup.Uint8(), request.ChannelType)
	require.Equal(t, 1, request.Header.RedDot)
	require.Equal(t, 1, cap.countAvatarChanged(), "one save should emit one system message")
}

func TestGroupUpdateNameDoesNotSendAvatarSystemMessage(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)
	cap := startIMCapture(t, ctx)
	r := newAvatarNotifyGroupUpdateRouter(t, ctx, g)

	const groupNo = "avatar_tip_name_only"
	seedCreatorGroup(t, g, groupNo, "原始群名")

	w := putGroupUpdate(t, r, groupNo, map[string]string{"name": "新群名"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Nil(t, cap.findAvatarChanged(), "rename must not emit avatar system message")
}

func TestGroupUpdateAvatarCustomSuppressesNoVisibleChange(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)
	cap := startIMCapture(t, ctx)
	r := newAvatarNotifyGroupUpdateRouter(t, ctx, g)

	const groupNo = "avatar_tip_no_visible_change"
	seedCreatorGroup(t, g, groupNo, "原始群名")
	model, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.NoError(t, g.db.updateAvatar("uploaded.png", model.AvatarVersion+1, groupNo))

	w := putGroupUpdate(t, r, groupNo, map[string]string{
		attrKeyAvatarText:  "研发",
		attrKeyAvatarColor: "5",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Zero(t, cap.countAvatarChanged(), "custom avatar fields hidden by uploaded avatar must not emit")
}

func TestGroupUpdateAvatarCustomClearsUploadedAvatarAndSendsSystemMessage(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)
	cap := startIMCapture(t, ctx)
	r := newAvatarNotifyGroupUpdateRouter(t, ctx, g)

	const groupNo = "avatar_tip_clear_upload"
	seedCreatorGroup(t, g, groupNo, "原始群名")
	model, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.NoError(t, g.db.updateAvatar("uploaded.png", model.AvatarVersion+1, groupNo))

	w := putGroupUpdate(t, r, groupNo, map[string]string{
		attrKeyAvatarText:          "研发",
		attrKeyAvatarColor:         "5",
		attrKeyClearUploadedAvatar: "true",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, cap.countAvatarChanged(), "clearing uploaded avatar should emit one system message")
}

func newAvatarNotifyGroupUpdateRouter(t *testing.T, ctx *config.Context, f *Group) http.Handler {
	t.Helper()
	r := wkhttp.New()
	grp := r.Group("/v1/groups", ctx.AuthMiddleware(r))
	grp.PUT("/:group_no", f.groupUpdate)
	return r
}

func TestGroupAvatarUploadSendsSystemMessage(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	cap := startIMCapture(t, ctx)

	f := New(ctx)
	f.fileService = &mockAvatarUploadFileService{}
	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "user-c", ShortNo: "uc_avatar_tip"}))
	const groupNo = "g-avatar-tip-upload"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "avatar tip upload",
		Creator: testutil.UID,
		Status:  GroupStatusNormal,
		Version: 1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo,
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Status:  1,
		Version: 1,
		Vercode: "avatar-tip-upload@1",
	}))

	r := wkhttp.New()
	r.POST("/v1/groups/:group_no/avatar", ctx.AuthMiddleware(r), f.avatarUpload)

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
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	payload := cap.findAvatarChanged()
	require.NotNil(t, payload, "avatar upload should emit 修改了群头像")
	require.Equal(t, float64(common.GroupUpdate), payload["type"])
	request := cap.findAvatarChangedRequest()
	require.NotNil(t, request)
	require.Equal(t, groupNo, request.ChannelID)
	require.Equal(t, common.ChannelTypeGroup.Uint8(), request.ChannelType)
	require.Equal(t, 1, request.Header.RedDot)
	extra, ok := payload["extra"].([]any)
	require.True(t, ok)
	require.Len(t, extra, 1)
	op, ok := extra[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, testutil.UID, op["uid"])
	require.Equal(t, "test", op["name"])
}
