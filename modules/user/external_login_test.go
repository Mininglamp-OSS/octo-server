package user

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupExternalLoginTest 准备一份纯净的 *User 以及它依赖的最小 DB / Context 状态。
//
// 处理三层 pre-existing 测试基础设施陷阱(详见 memory/feedback_user_db_test_pitfalls.md):
//  1. testutil.NewTestServer 内部已经 module.Setup → 注册了 user 路由,如果再
//     `u.Route(s.GetRoute())` 会触发 gin addRoute panic(同一路径重复注册)。
//  2. CleanAllTables 会 TRUNCATE app_config,而 sentWelcomeMsg 异步 goroutine
//     里 commonService.GetAppConfig 没做空表保护,空表会让 *appConfigResp 解引
//     用 nil 直接 SIGSEGV,且 goroutine panic 拦不住整个测试进程,断言看不到。
//     这里预先 INSERT 一行 app_config 兜底。
//  3. testutil.NewTestServer 没 wire ctx.Event(注释掉了),createUserWithRespAndTx
//     会调 ctx.EventBegin → c.Event.Begin → nil deref。手动注入 event.New(ctx)。
func setupExternalLoginTest(t *testing.T) *User {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	if ctx.Event == nil {
		ctx.Event = event.New(ctx)
	}

	// 兜底 app_config 行,避免 sentWelcomeMsg goroutine nil deref。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO app_config (rsa_private_key, rsa_public_key, version, super_token) VALUES ('', '', 1, '')",
	).Exec()
	require.NoError(t, err, "seed app_config")

	return New(ctx)
}

// 验证 *Service 在没有注入 handler 时返回 sentinel error。
// 不依赖测试服务器,纯 in-memory 验证保护 IService 契约不被悄悄破坏。
func TestService_LoginByExternalIdentity_NotConfigured(t *testing.T) {
	svc := &Service{}

	resp, err := svc.LoginByExternalIdentity(context.Background(), ExternalLoginReq{
		ExistingUID: "any",
	})
	assert.Nil(t, resp)
	assert.True(t, errors.Is(err, ErrExternalLoginNotConfigured), "expect ErrExternalLoginNotConfigured, got %v", err)
}

// LoginByExternalIdentity 已存在用户分支:用 table-driven 覆盖正常/拒绝/冷静期/缺失四种情况。
// 共享同一份 *User 实例,通过 t.Run 子用例前清表 + 重新插入避免相互污染。
func TestService_LoginByExternalIdentity_ExistingUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	u := setupExternalLoginTest(t)

	tests := []struct {
		name      string
		seed      *Model // nil 表示不预先插入用户
		req       ExternalLoginReq
		wantErr   bool
		assertOK  func(t *testing.T, resp *ExternalLoginResp)
		assertDB  func(t *testing.T, u *User)
	}{
		{
			name: "normal active user issues session",
			seed: &Model{
				UID: "ext-existing-uid-1", Username: "ext_existing", Name: "ExtExisting",
				ShortNo: "extshort1", Vercode: "ext-existing-uid-1@1", Status: int(common.UserAvailable),
			},
			req: ExternalLoginReq{ExistingUID: "ext-existing-uid-1", DeviceFlag: config.APP},
			assertOK: func(t *testing.T, resp *ExternalLoginResp) {
				assert.Equal(t, "ext-existing-uid-1", resp.UID)
				assert.False(t, resp.IsNewUser)
				assert.Contains(t, resp.LoginRespJSON, `"token":`)
				assert.Contains(t, resp.LoginRespJSON, `"uid":"ext-existing-uid-1"`)
			},
		},
		{
			name: "destroyed user is rejected",
			seed: &Model{
				UID: "ext-destroyed-uid", Username: "ext_destroyed", Name: "Gone",
				ShortNo: "extshort2", Vercode: "ext-destroyed-uid@1", Status: int(common.UserAvailable),
				IsDestroy: IsDestroyDone,
			},
			req:     ExternalLoginReq{ExistingUID: "ext-destroyed-uid", DeviceFlag: config.APP},
			wantErr: true,
		},
		{
			name: "cooling-off user can still login (PR #1192 product rule)",
			seed: &Model{
				UID: "ext-cooling-uid", Username: "ext_cooling", Name: "Cooling",
				ShortNo: "extshort3", Vercode: "ext-cooling-uid@1", Status: int(common.UserAvailable),
				IsDestroy: IsDestroyApplying,
			},
			req: ExternalLoginReq{ExistingUID: "ext-cooling-uid", DeviceFlag: config.APP},
			assertOK: func(t *testing.T, resp *ExternalLoginResp) {
				assert.Equal(t, "ext-cooling-uid", resp.UID)
			},
		},
		{
			name:    "missing UID returns error (no silent insert)",
			seed:    nil,
			req:     ExternalLoginReq{ExistingUID: "no-such-uid", DeviceFlag: config.APP},
			wantErr: true,
		},
		{
			name: "claims.Name change syncs to user.name (issue #1307)",
			seed: &Model{
				UID: "ext-sync-name-uid", Username: "ext_sync_name", Name: "OldName",
				ShortNo: "extsyncname", Vercode: "ext-sync-name-uid@1", Status: int(common.UserAvailable),
			},
			req: ExternalLoginReq{ExistingUID: "ext-sync-name-uid", Name: "NewName", DeviceFlag: config.APP},
			assertDB: func(t *testing.T, u *User) {
				got, err := u.db.QueryByUID("ext-sync-name-uid")
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, "NewName", got.Name, "OCTO 上游改名应同步到 IM")
			},
		},
		{
			name: "empty claims.Name does not overwrite existing name",
			seed: &Model{
				UID: "ext-empty-name-uid", Username: "ext_empty_name", Name: "KeepMe",
				ShortNo: "extempty", Vercode: "ext-empty-name-uid@1", Status: int(common.UserAvailable),
			},
			req: ExternalLoginReq{ExistingUID: "ext-empty-name-uid", Name: "", DeviceFlag: config.APP},
			assertDB: func(t *testing.T, u *User) {
				got, err := u.db.QueryByUID("ext-empty-name-uid")
				require.NoError(t, err)
				assert.Equal(t, "KeepMe", got.Name, "IdP 偶发不返 name 不应破坏已有数据")
			},
		},
		{
			// token cache key 用 `uid@name@role` 三段,name 含 @ 会让恶意 IdP 通过
			// 注入 `admin@0@admin` 之类伪造角色字段。GitHub/Gitee 路径(api_github.go:91、
			// api_gitee.go:162)都做了 ReplaceAll @ → _,OIDC 必须对齐。
			name: "name with @ is sanitized to _ on sync (privilege escalation guard)",
			seed: &Model{
				UID: "ext-at-uid", Username: "ext_at", Name: "OldName",
				ShortNo: "extat", Vercode: "ext-at-uid@1", Status: int(common.UserAvailable),
			},
			req: ExternalLoginReq{ExistingUID: "ext-at-uid", Name: "evil@0@admin", DeviceFlag: config.APP},
			assertDB: func(t *testing.T, u *User) {
				got, err := u.db.QueryByUID("ext-at-uid")
				require.NoError(t, err)
				assert.Equal(t, "evil_0_admin", got.Name, "@ 必须替换为 _ 防止 token cache 角色注入")
			},
		},
	}

	for _, tt := range tests {
		tt := tt // Go 1.20 循环变量被多次迭代复用同一地址,闭包里捕获 range var 有踩坑风险,显式 shadow 一份避免。
		t.Run(tt.name, func(t *testing.T) {
			// 子用例之间隔离:清 user 表(不动 app_config 那行兜底数据)。
			_, err := u.ctx.DB().DeleteFrom("user").Exec()
			require.NoError(t, err)

			if tt.seed != nil {
				require.NoError(t, u.db.Insert(tt.seed))
			}

			resp, err := u.userService.LoginByExternalIdentity(context.Background(), tt.req)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)

			if tt.assertOK != nil {
				tt.assertOK(t, resp)
			}
			if tt.assertDB != nil {
				tt.assertDB(t, u)
			}
		})
	}
}

// 新建用户路径同样必须做 @ 消毒(externalLoginCreate),不然 IdP 注入的 name
// 会直接进 createUserModel.Name → token cache key,绕开 ValidateName 守护。
func TestService_LoginByExternalIdentity_CreateUserSanitizesAtInName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	u := setupExternalLoginTest(t)

	uid := util.GenerUUID()
	resp, err := u.userService.LoginByExternalIdentity(context.Background(), ExternalLoginReq{
		UID:        uid,
		Name:       "evil@0@admin",
		Email:      "evil@example.com",
		DeviceFlag: config.APP,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	got, err := u.db.QueryByUID(uid)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "evil_0_admin", got.Name, "新建路径也必须 @ → _ 消毒")
	assert.Equal(t, "evil@example.com", got.Email, "email 字段不受影响")
}

// 空 ExistingUID + 非空 UID:走 createUserWithRespAndTx 路径,新建用户。
func TestService_LoginByExternalIdentity_CreateNewUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	u := setupExternalLoginTest(t)

	uid := util.GenerUUID()
	resp, err := u.userService.LoginByExternalIdentity(context.Background(), ExternalLoginReq{
		UID:        uid,
		Name:       "ExtNew",
		Email:      "ext.new@example.com",
		DeviceFlag: config.APP,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, uid, resp.UID)
	assert.True(t, resp.IsNewUser)
	assert.Contains(t, resp.LoginRespJSON, `"token":`)

	got, err := u.db.QueryByUID(uid)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ExtNew", got.Name)
	assert.Equal(t, "ext.new@example.com", got.Email)
}

// 新建用户但未传 UID 应明确报错(避免依赖隐式 UUID 生成导致绑定错乱)。
func TestService_LoginByExternalIdentity_CreateRequiresUID(t *testing.T) {
	u := setupExternalLoginTest(t)

	resp, err := u.userService.LoginByExternalIdentity(context.Background(), ExternalLoginReq{
		Name:       "MissingUID",
		DeviceFlag: config.APP,
	})
	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "UID is required")
}
