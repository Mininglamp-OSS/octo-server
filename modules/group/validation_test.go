package group

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGroupReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     groupReq
		wantErr bool
	}{
		{
			name:    "valid with members",
			req:     groupReq{Name: "测试群", Members: []string{"uid1", "uid2"}},
			wantErr: false,
		},
		{
			name:    "valid with single member",
			req:     groupReq{Name: "测试群", Members: []string{"uid1"}},
			wantErr: false,
		},
		{
			name:    "empty members",
			req:     groupReq{Name: "测试群", Members: []string{}},
			wantErr: true,
		},
		{
			name:    "nil members",
			req:     groupReq{Name: "测试群", Members: nil},
			wantErr: true,
		},
		{
			name:    "no name but has members",
			req:     groupReq{Name: "", Members: []string{"uid1"}},
			wantErr: false, // name 不是必填
		},
		{
			name:    "Project snapshot may omit request members",
			req:     groupReq{Name: "项目群", ProjectID: "project-1", SpaceID: "space-1"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "群成员不能为空")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMemberAddReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     memberAddReq
		wantErr bool
	}{
		{"valid", memberAddReq{Members: []string{"uid1"}}, false},
		{"multiple members", memberAddReq{Members: []string{"uid1", "uid2", "uid3"}}, false},
		{"empty", memberAddReq{Members: []string{}}, true},
		{"nil", memberAddReq{Members: nil}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "群成员不能为空")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMemberRemoveReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     memberRemoveReq
		wantErr bool
	}{
		{"valid", memberRemoveReq{Members: []string{"uid1"}}, false},
		{"multiple members", memberRemoveReq{Members: []string{"uid1", "uid2"}}, false},
		{"empty", memberRemoveReq{Members: []string{}}, true},
		{"nil", memberRemoveReq{Members: nil}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "群成员不能为空")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInviteReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     InviteReq
		wantErr bool
	}{
		{"valid", InviteReq{UIDS: []string{"uid1"}}, false},
		{"with remark", InviteReq{UIDS: []string{"uid1"}, Remark: "请加入"}, false},
		{"empty uids", InviteReq{UIDS: []string{}}, true},
		{"nil uids", InviteReq{UIDS: nil}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "被邀请者不能为空")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGroupStatusConstants(t *testing.T) {
	assert.Equal(t, 0, GroupStatusDisabled)
	assert.Equal(t, 1, GroupStatusNormal)
	assert.Equal(t, 2, GroupStatusDisband)
}

func TestMemberRoleConstants(t *testing.T) {
	assert.Equal(t, 0, MemberRoleCommon)
	assert.Equal(t, 1, MemberRoleCreator)
	assert.Equal(t, 2, MemberRoleManager)
}

func TestInviteStatusConstants(t *testing.T) {
	assert.Equal(t, 0, InviteStatusWait)
	assert.Equal(t, 1, InviteStatusOK)
}

func TestGroupTypeConstants(t *testing.T) {
	assert.Equal(t, GroupType(0), GroupTypeCommon)
	assert.Equal(t, GroupType(1), GroupTypeSuper)
}

func TestChannelServiceName(t *testing.T) {
	assert.Equal(t, "channel", ChannelServiceName)
}

func TestGroupReq_Check_NameVariants(t *testing.T) {
	tests := []struct {
		name    string
		req     groupReq
		wantErr bool
	}{
		{"long name with members", groupReq{Name: "很长很长的群组名称但是有成员", Members: []string{"uid1"}}, false},
		{"empty name with members", groupReq{Name: "", Members: []string{"uid1"}}, false},
		{"unicode name with members", groupReq{Name: "测试群🎉", Members: []string{"uid1"}}, false},
		{"many members", groupReq{Name: "群", Members: []string{"uid1", "uid2", "uid3", "uid4", "uid5"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBlacklistReq_Fields(t *testing.T) {
	req := blacklistReq{
		Uids: []string{"uid1", "uid2", "uid3"},
	}
	assert.Len(t, req.Uids, 3)
	assert.Equal(t, "uid1", req.Uids[0])
}

func TestBlacklistReq_Empty(t *testing.T) {
	req := blacklistReq{Uids: []string{}}
	assert.Empty(t, req.Uids)

	reqNil := blacklistReq{}
	assert.Nil(t, reqNil.Uids)
}

func TestGroupStatusValues_NoDuplicates(t *testing.T) {
	statuses := []int{GroupStatusDisabled, GroupStatusNormal, GroupStatusDisband}
	seen := make(map[int]bool)
	for _, s := range statuses {
		assert.False(t, seen[s], "duplicate status value: %d", s)
		seen[s] = true
	}
	assert.Len(t, seen, 3)
}

func TestMemberRoleValues_Order(t *testing.T) {
	roles := []int{MemberRoleCommon, MemberRoleCreator, MemberRoleManager}
	seen := make(map[int]bool)
	for _, r := range roles {
		assert.False(t, seen[r], "duplicate role value: %d", r)
		seen[r] = true
	}
	assert.Len(t, seen, 3)
	assert.Less(t, MemberRoleCommon, MemberRoleCreator)
}

func TestResolveGroupNo(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"regular group", "abc123def456", "abc123def456"},
		{"thread channel", "abc123____shortid", "abc123"},
		{"separator at start", "____shortid", "____shortid"},
		{"empty string", "", ""},
		{"separator only", "____", "____"},
		{"multiple separators", "abc____def____ghi", "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, resolveGroupNo(tt.input))
		})
	}
}

func TestInviteReq_Check_WithRemark(t *testing.T) {
	tests := []struct {
		name    string
		req     InviteReq
		wantErr bool
	}{
		{"long remark", InviteReq{UIDS: []string{"uid1"}, Remark: "这是一个很长的备注信息用于测试"}, false},
		{"empty remark", InviteReq{UIDS: []string{"uid1"}, Remark: ""}, false},
		{"many uids", InviteReq{UIDS: []string{"uid1", "uid2", "uid3", "uid4", "uid5"}, Remark: "邀请多人"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
