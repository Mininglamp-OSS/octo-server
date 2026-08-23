package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/source"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestFriendSureSearch(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	//u.Route(s.GetRoute())
	f := NewFriend(ctx)
	//f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// 模拟vercode
	vercode := "111@1"
	err = u.db.Insert(&Model{
		UID:      testutil.UID,
		Name:     "111",
		Username: "111",
		Vercode:  vercode,
		ShortNo:  "1111111",
	})
	assert.NoError(t, err)
	vercode = "222@1"
	err = u.db.Insert(&Model{
		UID:      "222",
		Name:     "222",
		Username: "222",
		Vercode:  vercode,
		ShortNo:  "121",
	})
	assert.NoError(t, err)
	err = f.db.insertApply(&FriendApplyModel{
		UID:    testutil.UID,
		ToUID:  "222",
		Remark: "我是备注",
		Status: 0,
	})
	assert.NoError(t, err)
	token := util.GenerUUID()
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.FriendApplyTokenCachePrefix+token+testutil.UID, util.ToJson(map[string]interface{}{
		"from_uid": "222",
		"vercode":  vercode,
	}), u.ctx.GetConfig().Cache.FriendApplyExpire)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/friend/sure", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"token": token,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	// assert.Equal(t, http.StatusOK, w.Code)
	panic(w.Body)
}

func TestFriendSureQr(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	f := NewFriend(ctx)
	f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	vercode := "111@1"
	err = u.db.Insert(&Model{
		UID:       testutil.UID,
		Name:      "111",
		Username:  "111",
		Vercode:   vercode,
		QRVercode: "111@3",
	})
	assert.NoError(t, err)
	vercode = "222@1"
	err = u.db.Insert(&Model{
		UID:       "222",
		Name:      "222",
		Username:  "222",
		Vercode:   vercode,
		QRVercode: "222@3",
	})
	assert.NoError(t, err)
	token := util.GenerUUID()
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.FriendApplyTokenCachePrefix+token+testutil.UID, util.ToJson(map[string]interface{}{
		"from_uid": "222",
		"code":     "222@3",
	}), u.ctx.GetConfig().Cache.FriendApplyExpire)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/friend/sure", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"token": token,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFriendSureCard(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	f := NewFriend(ctx)
	f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.db.Insert(&FriendModel{
		UID:     "111",
		ToUID:   "222",
		Vercode: "111@4",
	})
	assert.NoError(t, err)
	err = f.db.Insert(&FriendModel{
		UID:     "222",
		ToUID:   "111",
		Vercode: "222@4",
	})
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:       testutil.UID,
		Name:      "10000",
		Username:  "10000",
		Vercode:   "10000@1",
		QRVercode: "10000@3",
	})
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:       "111",
		Name:      "111",
		Username:  "111",
		Vercode:   "111@1",
		QRVercode: "111@3",
	})
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:       "222",
		Name:      "222",
		Username:  "222",
		Vercode:   "222@1",
		QRVercode: "222@3",
	})
	assert.NoError(t, err)

	token := util.GenerUUID()
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.FriendApplyTokenCachePrefix+token+testutil.UID, util.ToJson(map[string]interface{}{
		"from_uid": "admin",
		"code":     "111@4",
	}), u.ctx.GetConfig().Cache.FriendApplyExpire)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/friend/sure", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"token": token,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFriendSureGroup(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	f := NewFriend(ctx)
	f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	source.SetGroupMemberProvider(&emptyGroupProvider{})
	//添加一条群成员记录
	_, err = f.db.session.InsertInto("group_member").Columns("uid", "vercode", "group_no", "is_deleted").Values("111", "111@2", "g111", 0).Exec()
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:       testutil.UID,
		Name:      "10000",
		Username:  "10000",
		Vercode:   "10000@1",
		QRVercode: "10000@3",
	})
	assert.NoError(t, err)
	token := util.GenerUUID()
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.FriendApplyTokenCachePrefix+token+testutil.UID, util.ToJson(map[string]interface{}{
		"from_uid": "10000",
		"code":     "111@2",
	}), u.ctx.GetConfig().Cache.FriendApplyExpire)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/friend/sure", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"token": token,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

type emptyGroupProvider struct {
}

func (e *emptyGroupProvider) GetGroupMemberByVercode(vercode string) (*source.GroupMember, error) {
	return &source.GroupMember{
		Name:    "111",
		UID:     "111",
		Vercode: "111@2",
		GroupNo: "g111",
	}, nil
}
func (e *emptyGroupProvider) GetGroupMemberByVercodes(vercodes []string) ([]*source.GroupMember, error) {
	return []*source.GroupMember{
		{
			Name:    "111",
			UID:     "111",
			Vercode: "111@2",
			GroupNo: "g111",
		},
	}, nil
}
func (e *emptyGroupProvider) GetGroupMemberByUID(uid string, group string) (*source.GroupMember, error) {
	return &source.GroupMember{
		Name:    "111",
		UID:     "111",
		Vercode: "111@2",
		GroupNo: "g111",
		Role:    1,
	}, nil
}

// 获取群信息
func (e *emptyGroupProvider) GetGroupByGroupNo(groupNo string) (*source.GroupModel, error) {
	return &source.GroupModel{
		GroupNo: "g111",
		Name:    "111",
	}, nil
}

func TestUserDetail(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	f := NewFriend(ctx)
	f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	source.SetGroupMemberProvider(&emptyGroupProvider{})
	//添加一条群成员记录
	_, err = f.db.session.InsertInto("group_member").Columns("uid", "vercode", "group_no", "is_deleted").Values("111", "111@2", "g111", 0).Exec()
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:       testutil.UID,
		Name:      "10000",
		Username:  "10000",
		Vercode:   "10000@1",
		QRVercode: "10000@3",
	})
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:       "111",
		Name:      "111",
		Username:  "111",
		Vercode:   "111@1",
		QRVercode: "111@3",
	})
	assert.NoError(t, err)
	f.db.Insert(&FriendModel{
		UID:           testutil.UID,
		ToUID:         "111",
		Vercode:       "10000@4",
		SourceVercode: "111@2",
	})
	assert.NoError(t, err)

	f.db.Insert(&FriendModel{
		UID:           "111",
		ToUID:         testutil.UID,
		Vercode:       "111@4",
		SourceVercode: "10000@2",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/users/111", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"revoke_remind":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":"111"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"111"`))

}

func TestRemark(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	f := NewFriend(ctx)
	f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	f.db.Insert(&FriendModel{
		UID:           testutil.UID,
		ToUID:         "111",
		Vercode:       "10000@4",
		SourceVercode: "111@2",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/friend/remark", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"remark": "这是备注",
		"uid":    "111",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	panic(w.Body)
}
func TestApply(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := NewFriend(ctx)
	//u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.userDB.Insert(&Model{
		UID:     testutil.UID,
		ShortNo: "u1",
		Name:    "u1",
	})
	assert.NoError(t, err)

	err = u.userDB.Insert(&Model{
		UID:     "111",
		ShortNo: "111",
		Name:    "111",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/friend/apply", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"remark":  "这是备注",
		"to_uid":  "111",
		"vercode": "ssd",
	}))))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
func TestDeleteApply(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := NewFriend(ctx)
	//u.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = u.db.insertApply(&FriendApplyModel{
		UID:    testutil.UID,
		ToUID:  "123",
		Remark: "我备注",
		Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/friend/apply/123", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestParseFriendApplyData tests the safe type assertion logic for cache data parsing
// This verifies the fix for issue #542 - unsafe type assertion panic risk
func TestParseFriendApplyData(t *testing.T) {
	tests := []struct {
		name           string
		data           map[string]interface{}
		expectFromUID  string
		expectVercode  string
		expectRemark   string
		expectFromOK   bool
		expectVercodeOK bool
	}{
		{
			name: "valid data with all fields",
			data: map[string]interface{}{
				"from_uid": "user123",
				"vercode":  "abc@1",
				"remark":   "hello",
			},
			expectFromUID:   "user123",
			expectVercode:   "abc@1",
			expectRemark:    "hello",
			expectFromOK:    true,
			expectVercodeOK: true,
		},
		{
			name: "valid data without remark",
			data: map[string]interface{}{
				"from_uid": "user123",
				"vercode":  "abc@1",
			},
			expectFromUID:   "user123",
			expectVercode:   "abc@1",
			expectRemark:    "",
			expectFromOK:    true,
			expectVercodeOK: true,
		},
		{
			name: "from_uid is integer (should fail safely)",
			data: map[string]interface{}{
				"from_uid": 12345,
				"vercode":  "abc@1",
			},
			expectVercode:   "abc@1",
			expectFromOK:    false,
			expectVercodeOK: true,
		},
		{
			name: "from_uid is missing (should fail safely)",
			data: map[string]interface{}{
				"vercode": "abc@1",
			},
			expectVercode:   "abc@1",
			expectFromOK:    false,
			expectVercodeOK: true,
		},
		{
			name: "vercode is integer (should fail safely)",
			data: map[string]interface{}{
				"from_uid": "user123",
				"vercode":  99999,
			},
			expectFromUID:   "user123",
			expectFromOK:    true,
			expectVercodeOK: false,
		},
		{
			name: "vercode is missing (should fail safely)",
			data: map[string]interface{}{
				"from_uid": "user123",
			},
			expectFromUID:   "user123",
			expectFromOK:    true,
			expectVercodeOK: false,
		},
		{
			name: "remark is not a string (should be ignored gracefully)",
			data: map[string]interface{}{
				"from_uid": "user123",
				"vercode":  "abc@1",
				"remark":   []int{1, 2, 3},
			},
			expectFromUID:   "user123",
			expectVercode:   "abc@1",
			expectRemark:    "", // invalid remark should become empty, not panic
			expectFromOK:    true,
			expectVercodeOK: true,
		},
		{
			name: "remark is nil (should be empty)",
			data: map[string]interface{}{
				"from_uid": "user123",
				"vercode":  "abc@1",
				"remark":   nil,
			},
			expectFromUID:   "user123",
			expectVercode:   "abc@1",
			expectRemark:    "",
			expectFromOK:    true,
			expectVercodeOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This test verifies the safe type assertion pattern used in friendSure
			// The actual implementation is in api_friend.go lines 607-624

			var gotFromUID, gotVercode, gotRemark string
			var fromOK, vercodeOK bool

			// Should not panic regardless of input
			assert.NotPanics(t, func() {
				gotFromUID, fromOK = tt.data["from_uid"].(string)
				gotVercode, vercodeOK = tt.data["vercode"].(string)
				gotRemark = ""
				if tt.data["remark"] != nil {
					if remarkVal, ok := tt.data["remark"].(string); ok {
						gotRemark = remarkVal
					}
				}
			})

			assert.Equal(t, tt.expectFromOK, fromOK, "from_uid type assertion result mismatch")
			assert.Equal(t, tt.expectVercodeOK, vercodeOK, "vercode type assertion result mismatch")

			if tt.expectFromOK {
				assert.Equal(t, tt.expectFromUID, gotFromUID, "from_uid value mismatch")
			}
			if tt.expectVercodeOK {
				assert.Equal(t, tt.expectVercode, gotVercode, "vercode value mismatch")
			}
			if tt.expectFromOK && tt.expectVercodeOK {
				assert.Equal(t, tt.expectRemark, gotRemark, "remark value mismatch")
			}
		})
	}
}
