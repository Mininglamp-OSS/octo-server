package message

// 多端未读同步回归：web 端清未读后，离线的 app 端重连时必须能补拉到 unreadClear 命令。
//
// 背景（Mininglamp-OSS/octo-server）：clearConversationUnread 把已读游标推给 IM 之后，
// 再发一条 unreadClear CMD 通知同账号的其它端抹掉红点。这条 CMD 一度是 NoPersist 的，
// 在 IM 侧走实时投递（internal/usecase/message/send.go 的 NoPersist 分支）——不落库、
// 不进命令频道投影（internal/usecase/cmdsync/intent.go 的 isDurableCMDProjectionMessage
// 直接把 NoPersist 判 false）。结果是 app 在后台 / 息屏 / 断网时这条命令永久丢失，
// 客户端重连后走 pullCMDMessages（iOS：WKCMDManager）也补不回来，红点一直挂着。
//
// 本测试用真实 WuKongIM 走完整链路：清未读 → 以离线设备的身份调 /v1/message/sync
// （即客户端 pullCMDMessages 打的接口，最终落到 IM 的 cmdSync.Sync，只读命令频道里
// 的持久化消息），断言 unreadClear 拉得到。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

// resetUnreadCMDUIDRateLimit 清掉共享 UID 限流桶。桶存活在 Redis 里，
// CleanAllTables 不管它，连跑多个用例会被 2rps 掐掉（见 CLAUDE.md 限流一节）。
func resetUnreadCMDUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	keys, err := rds.Keys("ratelimit:uid:*").Result()
	if err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}
}

// pullOfflineCMDs 复刻客户端重连后的 pullCMDMessages：POST /v1/message/sync
// 拿 max_message_seq 之后的命令频道消息。
func pullOfflineCMDs(t *testing.T, s *server.Server, maxMessageSeq uint32) []*MsgSyncResp {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/message/sync", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"max_message_seq": maxMessageSeq,
		"limit":           100,
	}))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "message/sync 应当成功: %s", w.Body.String())

	var resps []*MsgSyncResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resps), "响应体: %s", w.Body.String())
	return resps
}

// findUnreadClearCMD 在拉回来的离线消息里找目标频道的 unreadClear 命令。
func findUnreadClearCMD(msgs []*MsgSyncResp, channelID string) *MsgSyncResp {
	for _, msg := range msgs {
		if msg.Payload == nil {
			continue
		}
		if cmd, _ := msg.Payload["cmd"].(string); cmd != common.CMDConversationUnreadClear {
			continue
		}
		param, _ := msg.Payload["param"].(map[string]interface{})
		if param == nil {
			continue
		}
		if id, _ := param["channel_id"].(string); id == channelID {
			return msg
		}
	}
	return nil
}

// TestClearUnreadCMDReachesOfflineDevice 覆盖「web 读了 → app 离线 → app 重连补拉」这条链路。
func TestClearUnreadCMDReachesOfflineDevice(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	resetUnreadCMDUIDRateLimit(t, ctx)

	// 对端 UID 每次运行都取唯一值：WuKongIM 的数据不随 CleanAllTables 清除，
	// 固定 UID 会让上一轮跑留在命令频道里的 unreadClear 被本轮误当成新命令。
	peerUID := fmt.Sprintf("peer-%d", time.Now().UnixNano())

	// 先让对端发一条 DM，制造真实的未读（也让 IM 侧建出该频道）。
	require.NoError(t, ctx.SendMessage(&config.MsgSendReq{
		FromUID:     peerUID,
		ChannelID:   testutil.UID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte(util.ToJson(map[string]interface{}{"type": 1, "content": "hi"})),
	}))

	// 离线设备的水位：清未读之前命令频道已有的最大 seq。
	var offlineSeq uint32
	for _, msg := range pullOfflineCMDs(t, s, 0) {
		if msg.MessageSeq > offlineSeq {
			offlineSeq = msg.MessageSeq
		}
	}

	// web 端点开会话 → 清未读。
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/coversation/clearUnread", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"channel_id":   peerUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"unread":       0,
	}))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "clearUnread 应当成功: %s", w.Body.String())

	// app 重连后补拉离线命令。
	cmds := pullOfflineCMDs(t, s, offlineSeq)
	got := findUnreadClearCMD(cmds, peerUID)
	require.NotNil(t, got,
		"离线设备重连后必须能补拉到 unreadClear 命令，否则红点只能靠用户在本机再读一次才消失；拉到的命令: %s",
		util.ToJson(cmds))

	param, _ := got.Payload["param"].(map[string]interface{})
	require.Equal(t, peerUID, param["channel_id"])
	require.EqualValues(t, common.ChannelTypePerson.Uint8(), param["channel_type"])

	// 必须落在登录用户自己的命令频道。SendCMD 不带 FromUID 时 IM 会把命令归到全局的
	// ____system____cmd，那条频道的 message_seq 与用户自己的命令频道各自独立递增，
	// 客户端用单一 max_message_seq 做水位就会漏拉（"____cmd" 是 IM 侧的派生后缀，
	// 见 WuKongIM internal/runtime/channelid.CommandChannelSuffix）。
	require.Equal(t, testutil.UID+"____cmd", got.ChannelID,
		"unreadClear 应落在 %s 自己的命令频道，实际: %s", testutil.UID, got.ChannelID)
}
