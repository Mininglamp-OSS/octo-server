package user

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRegisterReq_CheckRegister(t *testing.T) {
	tests := []struct {
		name    string
		req     registerReq
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid registration",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "123456",
			},
			wantErr: false,
		},
		{
			name: "empty name",
			req: registerReq{
				Name:     "",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "123456",
			},
			wantErr: true,
			errMsg:  "用户名不能为空",
		},
		{
			name: "whitespace only name",
			req: registerReq{
				Name:     "   ",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "123456",
			},
			wantErr: true,
			errMsg:  "用户名不能为空",
		},
		{
			name: "empty zone",
			req: registerReq{
				Name:     "张三",
				Zone:     "",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "123456",
			},
			wantErr: true,
			errMsg:  "区号不能为空",
		},
		{
			name: "empty phone",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "",
				Code:     "1234",
				Password: "123456",
			},
			wantErr: true,
			errMsg:  "手机号不能为空",
		},
		{
			name: "empty code",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "",
				Password: "123456",
			},
			wantErr: true,
			errMsg:  "验证码不能为空",
		},
		{
			name: "empty password",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "",
			},
			wantErr: true,
			errMsg:  "密码不能为空",
		},
		{
			// 长度/复杂度不再由 CheckRegister 负责：它已收窄为"必填项非空 + 名字格式"，
			// 密码强度移到 register handler 里的 ValidatePasswordStrength（策略闸门之后）。
			// 强度规则本身由 TestValidatePasswordStrength 覆盖，handler 侧由
			// TestRegisterRejectsWeakPassword 覆盖。
			name: "short password is no longer CheckRegister's concern",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "12345",
			},
			wantErr: false,
		},
		{
			name: "non-empty password passes the non-empty check",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "123456",
			},
			wantErr: false,
		},
		{
			name: "password longer than 6",
			req: registerReq{
				Name:     "张三",
				Zone:     "0086",
				Phone:    "13800138000",
				Code:     "1234",
				Password: "1234567890",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.CheckRegister()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.True(t, strings.Contains(err.Error(), tt.errMsg),
						"error should contain %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoginReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     loginReq
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid login",
			req:     loginReq{Username: "admin", Password: "123456"},
			wantErr: false,
		},
		{
			name:    "empty username",
			req:     loginReq{Username: "", Password: "123456"},
			wantErr: true,
			errMsg:  "用户名不能为空",
		},
		{
			name:    "whitespace username",
			req:     loginReq{Username: "  ", Password: "123456"},
			wantErr: true,
			errMsg:  "用户名不能为空",
		},
		{
			name:    "empty password",
			req:     loginReq{Username: "admin", Password: ""},
			wantErr: true,
			errMsg:  "密码不能为空",
		},
		{
			name:    "whitespace password",
			req:     loginReq{Username: "admin", Password: "   "},
			wantErr: true,
			errMsg:  "密码不能为空",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestApplyReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     applyReq
		wantErr bool
	}{
		{"valid apply", applyReq{ToUID: "uid_123"}, false},
		{"empty ToUID", applyReq{ToUID: ""}, true},
		{"whitespace ToUID", applyReq{ToUID: "  "}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "好友的ID不能为空")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSureReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     sureReq
		wantErr bool
	}{
		{"valid token", sureReq{Token: "abc123"}, false},
		{"empty token", sureReq{Token: ""}, true},
		{"whitespace token", sureReq{Token: "  "}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "token不能为空")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewUserResp(t *testing.T) {
	m := &Model{
		UID:     "uid_123",
		Name:    "张三",
		Vercode: "vc_456",
	}
	resp := newUserResp(m)
	assert.Equal(t, "uid_123", resp.UID)
	assert.Equal(t, "张三", resp.Name)
	assert.Equal(t, "vc_456", resp.Vercode)
}

func TestStatus_Int(t *testing.T) {
	assert.Equal(t, 0, StatusDisable.Int())
	assert.Equal(t, 1, StatusEnable.Int())
}

func TestConstants(t *testing.T) {
	assert.Equal(t, "login", Web3VerifyLogin)
	assert.Equal(t, "password", Web3VerifyPassword)
	assert.Equal(t, "customerService", CategoryCustomerService)
	assert.Equal(t, "system", CategorySystem)
	assert.Equal(t, "friendApply", UserRedDotCategoryFriendApply)
	assert.Equal(t, "lm-friends:", CacheKeyFriends)
}

func TestManagerLoginReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     managerLoginReq
		wantErr bool
		errMsg  string
	}{
		{"valid", managerLoginReq{Username: "admin", Password: "123456"}, false, ""},
		{"empty username", managerLoginReq{Username: "", Password: "123456"}, true, "用户名不能为空"},
		{"whitespace username", managerLoginReq{Username: "  ", Password: "123456"}, true, "用户名不能为空"},
		{"empty password", managerLoginReq{Username: "admin", Password: ""}, true, "密码不能为空"},
		{"whitespace password", managerLoginReq{Username: "admin", Password: "  "}, true, "密码不能为空"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestManagerAddUserReq_CheckAddUserReq(t *testing.T) {
	tests := []struct {
		name    string
		req     managerAddUserReq
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid",
			req:     managerAddUserReq{Name: "张三", Password: "123456", Phone: "13800138000"},
			wantErr: false,
		},
		{
			name:    "empty name",
			req:     managerAddUserReq{Name: "", Password: "123456", Phone: "13800138000"},
			wantErr: true,
			errMsg:  "用户名不能为空",
		},
		{
			name:    "empty password",
			req:     managerAddUserReq{Name: "张三", Password: "", Phone: "13800138000"},
			wantErr: true,
			errMsg:  "密码不能为空",
		},
		{
			name:    "empty phone",
			req:     managerAddUserReq{Name: "张三", Password: "123456", Phone: ""},
			wantErr: true,
			errMsg:  "手机号不能为空",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.checkAddUserReq()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNames(t *testing.T) {
	// Names 池不应为空
	assert.Greater(t, len(Names), 0, "Names pool should not be empty")
	// 应该有足够多的名字
	assert.Greater(t, len(Names), 50, "Names pool should have enough entries")
	// 每个名字不应为空
	for i, name := range Names {
		assert.NotEmpty(t, name, "Name at index %d should not be empty", i)
	}
}

func TestRegisterReq_CheckRegister_WhitespaceFields(t *testing.T) {
	tests := []struct {
		name    string
		req     registerReq
		wantErr bool
		errMsg  string
	}{
		{
			name: "tab in zone",
			req: registerReq{
				Name: "张三", Zone: "\t", Phone: "13800138000",
				Code: "1234", Password: "123456",
			},
			wantErr: true,
			errMsg:  "区号不能为空",
		},
		{
			name: "tab in phone",
			req: registerReq{
				Name: "张三", Zone: "0086", Phone: "\t",
				Code: "1234", Password: "123456",
			},
			wantErr: true,
			errMsg:  "手机号不能为空",
		},
		{
			name: "whitespace in code",
			req: registerReq{
				Name: "张三", Zone: "0086", Phone: "13800138000",
				Code: " \t ", Password: "123456",
			},
			wantErr: true,
			errMsg:  "验证码不能为空",
		},
		{
			name: "whitespace in password",
			req: registerReq{
				Name: "张三", Zone: "0086", Phone: "13800138000",
				Code: "1234", Password: "   ",
			},
			wantErr: true,
			errMsg:  "密码不能为空",
		},
		{
			name: "unicode name is valid",
			req: registerReq{
				Name: "用户🎉", Zone: "0086", Phone: "13800138000",
				Code: "1234", Password: "123456",
			},
			wantErr: false,
		},
		{
			name: "long password is valid",
			req: registerReq{
				Name: "张三", Zone: "0086", Phone: "13800138000",
				Code: "1234", Password: strings.Repeat("a", 100),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.CheckRegister()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoginReq_Check_WithDevice(t *testing.T) {
	// 测试带设备信息的登录请求
	req := loginReq{
		Username: "admin",
		Password: "123456",
		Flag:     0,
		Device: &deviceReq{
			DeviceID:    "device_001",
			DeviceName:  "iPhone 15",
			DeviceModel: "iPhone15,2",
		},
	}
	assert.NoError(t, req.Check())

	// PC flag
	reqPC := loginReq{
		Username: "admin",
		Password: "123456",
		Flag:     1,
	}
	assert.NoError(t, reqPC.Check())
}

func TestNewUserResp_EmptyFields(t *testing.T) {
	m := &Model{
		UID:     "",
		Name:    "",
		Vercode: "",
	}
	resp := newUserResp(m)
	assert.Equal(t, "", resp.UID)
	assert.Equal(t, "", resp.Name)
	assert.Equal(t, "", resp.Vercode)
}

func TestBlacklistResp_Fields(t *testing.T) {
	resp := blacklistResp{
		UID:      "uid_001",
		Name:     "张三",
		Username: "zhangsan",
	}
	assert.Equal(t, "uid_001", resp.UID)
	assert.Equal(t, "张三", resp.Name)
	assert.Equal(t, "zhangsan", resp.Username)
}

func TestDeviceReq_Fields(t *testing.T) {
	d := deviceReq{
		DeviceID:    "device_001",
		DeviceName:  "iPhone 15 Pro",
		DeviceModel: "iPhone15,2",
	}
	assert.Equal(t, "device_001", d.DeviceID)
	assert.Equal(t, "iPhone 15 Pro", d.DeviceName)
	assert.Equal(t, "iPhone15,2", d.DeviceModel)
}

func TestChatPwdReq_Fields(t *testing.T) {
	req := chatPwdReq{
		ChatPwd:  "111111",
		LoginPwd: "123456",
	}
	assert.Equal(t, "111111", req.ChatPwd)
	assert.Equal(t, "123456", req.LoginPwd)
}

func TestCodeReq_Fields(t *testing.T) {
	req := codeReq{
		Zone:  "0086",
		Phone: "13800138000",
	}
	assert.Equal(t, "0086", req.Zone)
	assert.Equal(t, "13800138000", req.Phone)
}

func TestRemarkReq_Fields(t *testing.T) {
	req := remarkReq{
		UID:    "uid_001",
		Remark: "老王",
	}
	assert.Equal(t, "uid_001", req.UID)
	assert.Equal(t, "老王", req.Remark)
}

func TestManagerLoginResp_Fields(t *testing.T) {
	resp := managerLoginResp{
		UID:   "uid_admin",
		Token: "token_abc",
		Name:  "管理员",
		Role:  "admin",
	}
	assert.Equal(t, "uid_admin", resp.UID)
	assert.Equal(t, "token_abc", resp.Token)
	assert.Equal(t, "管理员", resp.Name)
	assert.Equal(t, "admin", resp.Role)
}

func TestSetting_Fields(t *testing.T) {
	s := setting{
		SearchByPhone:     1,
		SearchByShort:     1,
		NewMsgNotice:      1,
		MsgShowDetail:     1,
		VoiceOn:           1,
		ShockOn:           1,
		OfflineProtection: 0,
		DeviceLock:        0,
		MuteOfApp:         0,
	}
	assert.Equal(t, 1, s.SearchByPhone)
	assert.Equal(t, 1, s.SearchByShort)
	assert.Equal(t, 1, s.NewMsgNotice)
	assert.Equal(t, 1, s.MsgShowDetail)
	assert.Equal(t, 1, s.VoiceOn)
	assert.Equal(t, 1, s.ShockOn)
	assert.Equal(t, 0, s.OfflineProtection)
	assert.Equal(t, 0, s.DeviceLock)
	assert.Equal(t, 0, s.MuteOfApp)
}
