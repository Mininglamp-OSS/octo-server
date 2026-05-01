package user

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGroupMemberExternalProvider_RegisterAndGet 覆盖注册 / 获取 / 回调返回值链路。
func TestGroupMemberExternalProvider_RegisterAndGet(t *testing.T) {
	setGroupMemberExternalProvider(nil)
	assert.Nil(t, getGroupMemberExternalProvider())

	RegisterGroupMemberExternalProvider(func(groupNo, uid string) (int, string, string, string, string, error) {
		if groupNo == "g-ext" && uid == "u1" {
			return 1, "src-space", "Src Space", "src-space", "Src Space", nil
		}
		return 0, "", "", "home-space", "Home Space", nil
	})
	defer setGroupMemberExternalProvider(nil)

	fn := getGroupMemberExternalProvider()
	assert.NotNil(t, fn)

	// 外部成员：home = source
	isExt, srcID, srcName, homeID, homeName, err := fn("g-ext", "u1")
	assert.NoError(t, err)
	assert.Equal(t, 1, isExt)
	assert.Equal(t, "src-space", srcID)
	assert.Equal(t, "Src Space", srcName)
	assert.Equal(t, "src-space", homeID)
	assert.Equal(t, "Src Space", homeName)

	// 内部成员：home = 群自身 space
	isExt, srcID, srcName, homeID, homeName, err = fn("g-internal", "u2")
	assert.NoError(t, err)
	assert.Equal(t, 0, isExt)
	assert.Equal(t, "", srcID)
	assert.Equal(t, "", srcName)
	assert.Equal(t, "home-space", homeID)
	assert.Equal(t, "Home Space", homeName)
}

// TestGroupMemberExternalProvider_ConcurrentAccess 验证读写并发安全（配合 -race）。
func TestGroupMemberExternalProvider_ConcurrentAccess(t *testing.T) {
	setGroupMemberExternalProvider(nil)
	defer setGroupMemberExternalProvider(nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			RegisterGroupMemberExternalProvider(func(groupNo, uid string) (int, string, string, string, string, error) {
				return 0, "", "", "", "", nil
			})
		}()
		go func() {
			defer wg.Done()
			if fn := getGroupMemberExternalProvider(); fn != nil {
				_, _, _, _, _, _ = fn("g", "u")
			}
		}()
	}
	wg.Wait()
}

// TestGroupMemberResp_JSONSerialization 锁定 GroupMemberResp 的 5 个新字段
// JSON 键名，与 /groups/{no}/members memberDetailResp 对齐，前端
// Web/Android/iOS UserInfo 依赖这些 key 做 Space 模式判定。
func TestGroupMemberResp_JSONSerialization(t *testing.T) {
	resp := GroupMemberResp{
		UID:             "u1",
		GroupNo:         "g1",
		Name:            "Alice",
		IsExternal:      1,
		SourceSpaceID:   "src-space-id",
		SourceSpaceName: "Src Space",
		HomeSpaceID:     "src-space-id",
		HomeSpaceName:   "Src Space",
	}
	b, err := json.Marshal(resp)
	assert.NoError(t, err)

	var decoded map[string]interface{}
	assert.NoError(t, json.Unmarshal(b, &decoded))
	assert.EqualValues(t, 1, decoded["is_external"])
	assert.Equal(t, "src-space-id", decoded["source_space_id"])
	assert.Equal(t, "Src Space", decoded["source_space_name"])
	assert.Equal(t, "src-space-id", decoded["home_space_id"])
	assert.Equal(t, "Src Space", decoded["home_space_name"])
}

// TestGroupMemberResp_DefaultsToAbsent 验证内部成员（未显式设置 is_external）序列化
// 仍保留零值字段，客户端按 "is_external==0 && home_space_id==group.space_id" 判定
// 同 Space 非好友分支，不依赖字段缺省。
func TestGroupMemberResp_DefaultsToAbsent(t *testing.T) {
	resp := GroupMemberResp{
		UID:           "u1",
		GroupNo:       "g1",
		Name:          "Bob",
		HomeSpaceID:   "home-space-id",
		HomeSpaceName: "Home Space",
	}
	b, err := json.Marshal(resp)
	assert.NoError(t, err)

	var decoded map[string]interface{}
	assert.NoError(t, json.Unmarshal(b, &decoded))
	assert.EqualValues(t, 0, decoded["is_external"])
	assert.Equal(t, "", decoded["source_space_id"])
	assert.Equal(t, "home-space-id", decoded["home_space_id"])
	assert.Equal(t, "Home Space", decoded["home_space_name"])
}
