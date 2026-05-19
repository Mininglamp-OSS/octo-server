package user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	common "github.com/Mininglamp-OSS/octo-server/modules/common"
	"go.uber.org/zap"
)

// sanitizeExternalName 把 IdP 返回的 name 里的 @ 字符替换为 _。
//
// token cache key 用 `uid@name@role` 三段,name 中的 @ 会让恶意 IdP 通过类似
// `admin@0@admin` 的取值伪造角色字段实现权限提升。GitHub/Gitee 路径
// (api_github.go:91、api_gitee.go:162)沿用此约定,OIDC 也走同样逻辑。
//
// 用替换而非拒绝,是因为 SSO 场景下 name 是 IdP 控制的,登录失败比静默清洗更糟。
func sanitizeExternalName(name string) string {
	return strings.ReplaceAll(name, "@", "_")
}

// ExternalLoginReq external IdP（OIDC / OAuth）登录入参。
//
// ExistingUID 为空表示按 IdP 返回的 claims 新建本地用户;非空则按已知 UID 登录。
// 调用方（oidc 模块的 ResolveOrLink）负责完成 (issuer, sub) → uid 的解析与绑定，
// 这里只负责签发 DMWork 会话 token + 推 WuKongIM。
type ExternalLoginReq struct {
	ExistingUID string

	// UID 仅新建用户场景下使用,ExistingUID 非空时忽略。
	UID string // 调用方生成的 UID（避免重复 GenerUUID 后还要再回传）

	// Name 在两条路径都用:新建时写入 user.name;ExistingUID 非空时与库中
	// user.name 比较,不同则同步覆盖(issue #1307)。两条路径都会做 @ → _ 消毒。
	Name  string
	Email string
	Phone string
	Zone  string

	DeviceFlag config.DeviceFlag
	Device     *DeviceInfo

	// PublicIP 用于欢迎消息日志,可空
	PublicIP string
}

// DeviceInfo 登录设备信息（外部模块用,与内部 deviceReq 解耦）
type DeviceInfo struct {
	DeviceID    string
	DeviceName  string
	DeviceModel string
}

// ExternalLoginResp 外部登录结果。
//
// LoginRespJSON 是 loginUserDetailResp 序列化后的 JSON 字符串,可直接落到
// ThirdAuthcode Redis 缓冲区供前端短码轮询取走;调用方无需关心其内部结构。
type ExternalLoginResp struct {
	UID           string
	IsNewUser     bool
	LoginRespJSON string
}

// LoginByExternalIdentity 给外部 IdP（OIDC / OAuth）登录流程签发 DMWork 会话。
//
// ExistingUID 非空 → 走 execLogin（已有用户）；
// ExistingUID 为空 → 走 createUserWithRespAndTx（创建用户 + 登录,事务内）。
//
// 行为复用 GitHub 登录路径（api_github.go），oidc 模块通过 IService 间接调用。
func (u *User) LoginByExternalIdentity(ctx context.Context, req ExternalLoginReq) (*ExternalLoginResp, error) {
	if req.ExistingUID != "" {
		return u.externalLoginExisting(ctx, req)
	}
	return u.externalLoginCreate(ctx, req)
}

func (u *User) externalLoginExisting(ctx context.Context, req ExternalLoginReq) (*ExternalLoginResp, error) {
	userInfoM, err := u.db.QueryByUID(req.ExistingUID)
	if err != nil {
		return nil, fmt.Errorf("user: query existing user uid=%s: %w", req.ExistingUID, err)
	}
	if userInfoM == nil {
		return nil, errors.New("用户不存在")
	}
	// IsDestroy 三态(db.go:15):0=正常 1=冷静期(可撤销) 2=已注销(终态)。
	// 冷静期用户允许登录,登录动作即撤销注销;已注销用户拒绝。
	// 与 api_emaillogin.go:245 / api.go:1012 等其他登录入口对齐。
	if userInfoM.IsDestroy == IsDestroyDone {
		return nil, errors.New("用户不存在")
	}

	// 重复登录:IdP 返回的 name 与库中不一致时同步覆盖,保证 OCTO 改名能反映到 IM。
	// 仅在 IdP 明确给了非空 name 时才动 — 偶发不返 name 不应破坏已有数据(issue #1307)。
	// 必须先消毒再比较,否则 IdP 反复给 `evil@0@admin` 会每次都触发 update。
	if req.Name != "" {
		newName := sanitizeExternalName(req.Name)
		if newName != userInfoM.Name {
			if err := u.db.UpdateUsersWithField("name", newName, req.ExistingUID); err != nil {
				// 名字同步失败不阻断登录,记 warn 让运维事后追溯。
				u.Warn("OIDC 重复登录同步 name 失败",
					zap.String("uid", req.ExistingUID),
					zap.String("old_name", userInfoM.Name),
					zap.String("new_name", newName),
					zap.Error(err))
			} else {
				userInfoM.Name = newName
			}
		}
	}

	loginResp, err := u.execLogin(userInfoM, req.DeviceFlag, toDeviceReq(req.Device), ctx)
	if err != nil {
		return nil, err
	}
	go u.sentWelcomeMsg(req.PublicIP, userInfoM.UID)

	return &ExternalLoginResp{
		UID:           userInfoM.UID,
		IsNewUser:     false,
		LoginRespJSON: util.ToJson(loginResp),
	}, nil
}

func (u *User) externalLoginCreate(ctx context.Context, req ExternalLoginReq) (*ExternalLoginResp, error) {
	if common.EnsureSystemSettings(u.ctx).RegisterOff() {
		return nil, errors.New("注册通道暂不开放")
	}
	if req.UID == "" {
		return nil, errors.New("user: external login: UID is required when creating new user")
	}

	tx, err := u.ctx.DB().Begin()
	if err != nil {
		return nil, fmt.Errorf("user: external login begin tx: %w", err)
	}
	// 与 githubOAuth 一致:仅 panic 时回滚,正常路径由下方 commit/rollback 显式控制
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in LoginByExternalIdentity: %v\n%s\n", r, debug.Stack())
		}
	}()

	createUser := &createUserModel{
		UID:    req.UID,
		Name:   sanitizeExternalName(req.Name), // 消毒同 externalLoginExisting,防 token cache key 注入
		Email:  req.Email,
		Phone:  req.Phone,
		Zone:   req.Zone,
		Flag:   int(req.DeviceFlag.Uint8()),
		Device: toDeviceReq(req.Device),
	}

	loginResp, err := u.createUserWithRespAndTx(ctx, createUser, req.PublicIP, nil, tx, func() error {
		if commitErr := tx.Commit(); commitErr != nil {
			tx.Rollback()
			u.Error("数据库事务提交失败", zap.Error(commitErr))
			return fmt.Errorf("user: external login commit tx: %w", commitErr)
		}
		return nil
	})
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	return &ExternalLoginResp{
		UID:           req.UID,
		IsNewUser:     true,
		LoginRespJSON: util.ToJson(loginResp),
	}, nil
}

func toDeviceReq(d *DeviceInfo) *deviceReq {
	if d == nil {
		return nil
	}
	return &deviceReq{
		DeviceID:    d.DeviceID,
		DeviceName:  d.DeviceName,
		DeviceModel: d.DeviceModel,
	}
}
