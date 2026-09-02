package oidc

import (
	"context"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// userAdapter 适配 user.IService + oidc.DB → service.userLookup。
//
// 把跨模块依赖收敛在这一层,service 测试只对接小接口 fakeUserLookup,
// 生产路径下 Init() 通过 register.GetService("user") 获取 userSvc 后注入本适配器。
type userAdapter struct {
	userSvc user.IService
	db      *DB
}

// newUserAdapter 构造生产路径的 userLookup 实现。
// userSvc 必须是已注入 ExternalLoginHandler 的实例(由 user.New 完成),
// 通常通过 register.GetService("user") 获取。
func newUserAdapter(userSvc user.IService, db *DB) *userAdapter {
	return &userAdapter{
		userSvc: userSvc,
		db:      db,
	}
}

func (a *userAdapter) UIDsByEmail(email string) ([]string, error) {
	return a.db.QueryUIDsByEmail(email)
}

func (a *userAdapter) UIDsByPhone(zone, phone string) ([]string, error) {
	return a.db.QueryUIDsByPhone(zone, phone)
}

// IssueSession 把 oidc IssueSessionReq 翻成 user.ExternalLoginReq。
//
// CreateUser=true 时 UID 必须在此处生成(user 模块不再回退分配),
// 这样 callback 拿到 UID 就能立刻写 identity 绑定行,不依赖 user 的回填。
func (a *userAdapter) IssueSession(ctx context.Context, req IssueSessionReq) (*IssueSessionResp, error) {
	uid := req.UID
	if req.CreateUser && uid == "" {
		uid = util.GenerUUID()
	}
	extReq := user.ExternalLoginReq{
		ExistingUID:      req.UID, // 已有用户;CreateUser=true 时由 user 模块判空走创建
		UID:              uid,
		Name:             req.Name,
		Email:            req.Email,
		Phone:            req.Phone,
		Zone:             req.Zone,
		DeviceFlag:       config.DeviceFlag(req.DeviceFlag),
		PublicIP:         req.PublicIP,
		TrustedSSOCreate: req.TrustedSSOCreate,
	}
	if req.CreateUser {
		extReq.ExistingUID = ""
	}
	if req.DeviceID != "" {
		extReq.Device = &user.DeviceInfo{
			DeviceID:    req.DeviceID,
			DeviceName:  req.DeviceName,
			DeviceModel: req.DeviceMod,
		}
	}
	resp, err := a.userSvc.LoginByExternalIdentity(ctx, extReq)
	if err != nil {
		return nil, fmt.Errorf("oidc: user.LoginByExternalIdentity: %w", err)
	}
	return &IssueSessionResp{
		UID:           resp.UID,
		IsNewUser:     resp.IsNewUser,
		LoginRespJSON: resp.LoginRespJSON,
	}, nil
}

// identityStoreAdapter 把 *DB 适配到 service.identityStore 小接口。
type identityStoreAdapter struct{ db *DB }

func (a identityStoreAdapter) Get(issuer, subject string) (*IdentityModel, error) {
	row, err := a.db.QueryIdentityByIssuerSubject(issuer, subject)
	if err != nil {
		return nil, err
	}
	// 逐字节复核 —— 见 identity_exact_match.go。表的 collation 是 ci,所以一次
	// 大小写不同的 subject 也会命中别人的行;那不是脏数据,是账号接管。
	if !identityRowMatches(row, issuer, subject) {
		return nil, nil
	}
	return row, nil
}
func (a identityStoreAdapter) Insert(m *IdentityModel) error {
	return a.db.InsertIdentity(m)
}
func (a identityStoreAdapter) UpdateLogin(id int64, email string, emailVerified int, phone string, phoneVerified int) error {
	return a.db.UpdateIdentityLogin(id, email, emailVerified, phone, phoneVerified)
}
