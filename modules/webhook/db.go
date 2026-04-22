package webhook

import (
	"github.com/gocraft/dbr/v2"
)

// DB DB
type DB struct {
	session *dbr.Session
}

// NewDB NewDB
func NewDB(session *dbr.Session) *DB {
	return &DB{
		session: session,
	}
}

// GetThirdName 获取三个名字 （常用名字，好友备注，群内名字） （TODO: 此方法不应该直接写sql 应该调用各模块的server来获取数据）
func (db *DB) GetThirdName(fromUID string, toUID string, groupNo string) (string, string, string, error) {
	if fromUID == "" {
		return "", "", "", nil
	}
	var name string        // 常用名
	var remark string      // 好友备注
	var nameInGroup string // 群内备注

	if toUID == "" && groupNo == "" {
		_, err := db.session.Select("name").From("`user`").Where("uid=?", fromUID).Load(&name)
		if err != nil {
			return "", "", "", nil
		}
	} else if toUID != "" && groupNo == "" {
		var nameStruct struct {
			Name   string
			Remark string
		}
		builder := db.session.SelectBySql("select `user`.name,IFNULL(friend.remark,'') remark from `user` left join friend on `user`.uid=friend.to_uid and friend.uid=? where `user`.uid=? ", toUID, fromUID)
		_, err := builder.Load(&nameStruct)
		if err != nil {
			return "", "", "", err
		}
		name = nameStruct.Name
		remark = nameStruct.Remark
	} else if toUID == "" && groupNo != "" {
		var nameStruct struct {
			Name        string
			NameInGroup string
		}
		_, err := db.session.SelectBySql("select `user`.name,IFNULL(group_member.remark,'') name_in_group from `user` left join group_member on group_member.group_no=?  and `user`.uid=group_member.uid and group_member.is_deleted=0 where `user`.uid=? ", groupNo, fromUID).Load(&nameStruct)
		if err != nil {
			return "", "", "", err
		}
		name = nameStruct.Name
		nameInGroup = nameStruct.NameInGroup
	} else if toUID != "" && groupNo != "" {
		var nameStruct struct {
			Name        string
			Remark      string
			NameInGroup string
		}
		_, err := db.session.SelectBySql("select `user`.name,IFNULL(group_member.remark,'') name_in_group,IFNULL(friend.remark ,'') remark from `user` left join group_member on  group_member.group_no=?  and `user`.uid=group_member.uid and group_member.is_deleted=0 left join friend on `user`.uid=friend.to_uid and `user`.uid=? and friend.uid=? where `user`.uid=?", groupNo, fromUID, toUID, fromUID).Load(&nameStruct)
		if err != nil {
			return "", "", "", err
		}
		name = nameStruct.Name
		nameInGroup = nameStruct.NameInGroup
		remark = nameStruct.Remark
	}
	return name, remark, nameInGroup, nil
}

// isRobot 判断uid是否是启用状态的Bot
func (db *DB) isRobot(uid string) (bool, error) {
	var cn int
	err := db.session.Select("count(*)").From("robot").Where("robot_id=? and status=1", uid).LoadOne(&cn)
	return cn > 0, err
}

// GetGroupName 获取群名
func (db *DB) GetGroupName(groupNo string) (string, error) {
	var name string
	_, err := db.session.Select("name").From("`group`").Where("group_no=?", groupNo).Load(&name)
	return name, err
}

// GetThreadName 获取子区名称
func (db *DB) GetThreadName(groupNo, shortID string) (string, error) {
	var name string
	_, err := db.session.Select("name").From("thread").Where("group_no=? AND short_id=?", groupNo, shortID).Load(&name)
	return name, err
}

// QueryThreadSettingsWithUIDs 批量查询一批用户对某子区的设置(推送免打扰判断用)
func (db *DB) QueryThreadSettingsWithUIDs(groupNo, shortID string, uids []string) ([]*threadSettingResp, error) {
	if len(uids) == 0 {
		return []*threadSettingResp{}, nil
	}
	var settings []*threadSettingResp
	_, err := db.session.Select("uid", "mute").From("thread_setting").
		Where("group_no=? AND short_id=? AND uid IN ?", groupNo, shortID, uids).Load(&settings)
	return settings, err
}
