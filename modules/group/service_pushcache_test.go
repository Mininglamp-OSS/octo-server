package group

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/pushcache"
	"github.com/stretchr/testify/assert"
)

// TestUpdateGroupInfo_InvalidatesPushNameCache 验证群改名后，离线推送标题缓存
// (pushcache.GroupNameKey) 被主动失效，避免手机推送在 TTL 到期前一直沿用旧群名。
func TestUpdateGroupInfo_InvalidatesPushNameCache(t *testing.T) {
	svc, _, ctx := setupServiceTestWithCtx(t)

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	err := NewDB(ctx).Insert(&Model{GroupNo: groupNo, Name: "旧群名", Creator: testutil.UID, Status: 1, Version: 1})
	assert.NoError(t, err)

	// 预置一条旧群名缓存，模拟此前推送已写入缓存的状态。
	key := pushcache.GroupNameKey(groupNo)
	assert.NoError(t, ctx.GetRedisConn().Set(key, "旧群名"))

	newName := "新群名"
	err = svc.UpdateGroupInfo(&UpdateGroupInfoServiceReq{
		GroupNo:      groupNo,
		OperatorUID:  testutil.UID,
		OperatorName: "操作者",
		Name:         &newName,
	})
	assert.NoError(t, err)

	// 改名后缓存应被删除，下一次推送会回源拿到新群名。
	cached, err := ctx.GetRedisConn().GetString(key)
	assert.NoError(t, err)
	assert.Empty(t, cached, "群改名后应失效推送群名缓存")
}

// TestUpdateGroupInfo_NoticeOnlyKeepsPushNameCache 验证只改公告(不改名)时不会动群名缓存，
// 避免无谓的回源。
func TestUpdateGroupInfo_NoticeOnlyKeepsPushNameCache(t *testing.T) {
	svc, _, ctx := setupServiceTestWithCtx(t)

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	err := NewDB(ctx).Insert(&Model{GroupNo: groupNo, Name: "群名", Creator: testutil.UID, Status: 1, Version: 1})
	assert.NoError(t, err)

	key := pushcache.GroupNameKey(groupNo)
	assert.NoError(t, ctx.GetRedisConn().Set(key, "群名"))

	notice := "新公告"
	err = svc.UpdateGroupInfo(&UpdateGroupInfoServiceReq{
		GroupNo:      groupNo,
		OperatorUID:  testutil.UID,
		OperatorName: "操作者",
		Notice:       &notice,
	})
	assert.NoError(t, err)

	cached, err := ctx.GetRedisConn().GetString(key)
	assert.NoError(t, err)
	assert.Equal(t, "群名", cached, "仅改公告不应失效群名缓存")
}
