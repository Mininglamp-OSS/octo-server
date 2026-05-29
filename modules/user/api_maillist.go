package user

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// 上传用户通讯录好友
func (u *User) addMaillist(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req []*mailListReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	result := make([]*mailListResp, 0)
	if len(req) == 0 {
		c.Response(result)
		return
	}
	loginUser, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询登录用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	tx, err := u.db.session.Begin()
	if err != nil {
		u.Error("数据库事物开启失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()

	for _, maillist := range req {
		zone := maillist.Zone
		if maillist.Zone == "" && !strings.HasPrefix(maillist.Phone, "00") {
			zone = loginUser.Zone
		}
		err := u.maillistDB.insertTx(&maillistModel{
			UID:     loginUID,
			Name:    maillist.Name,
			Zone:    zone,
			Phone:   maillist.Phone,
			Vercode: fmt.Sprintf("%s@%d", util.GenerUUID(), common.MailList),
		}, tx)
		if err != nil {
			tx.RollbackUnlessCommitted()
			u.Error("添加用户通讯录联系人错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}
	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		u.Error("数据库事物提交失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	c.ResponseOK()
}

// 获取用户通讯录好友
func (u *User) getMailList(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	result := make([]*mailListResp, 0)
	mailLists, err := u.maillistDB.query(loginUID)
	if err != nil {
		u.Error("查询用户通讯录数据错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if mailLists == nil {
		c.Response(result)
		return
	}
	phones := make([]string, 0)
	for _, m := range mailLists {
		phones = append(phones, fmt.Sprintf("%s%s", m.Zone, m.Phone))
	}
	users, err := u.db.QueryByPhones(phones)
	if err != nil {
		u.Error("批量查询用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	friends, err := u.friendDB.QueryFriends(loginUID)
	if err != nil {
		u.Error("查询用户好友错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	for _, m := range mailLists {
		var uid = ""
		for _, user := range users {
			if user.Zone == m.Zone && user.Phone == m.Phone {
				uid = user.UID
				break
			}
		}
		if uid == "" {
			continue
		}
		var isFriend = 0
		for _, friend := range friends {
			if uid != "" && friend.ToUID == uid {
				isFriend = 1
				break
			}
		}
		result = append(result, &mailListResp{
			Vercode:  m.Vercode,
			Phone:    m.Phone,
			Name:     m.Name,
			Zone:     m.Zone,
			UID:      uid,
			IsFriend: isFriend,
		})
	}
	c.Response(result)
}

type mailListReq struct {
	Name  string `json:"name"`
	Zone  string `json:"zone"`
	Phone string `json:"phone"`
}

type mailListResp struct {
	Name     string `json:"name"`
	Zone     string `json:"zone"`
	Phone    string `json:"phone"`
	UID      string `json:"uid"`
	Vercode  string `json:"vercode"`
	IsFriend int    `json:"is_friend"`
}
