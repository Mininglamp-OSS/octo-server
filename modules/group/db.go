package group

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// DB DB
type DB struct {
	ctx     *config.Context
	session *dbr.Session
}

// NewDB NewDB
func NewDB(ctx *config.Context) *DB {
	return &DB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// InsertTx 插入群信息（含事务）
func (d *DB) InsertTx(m *Model, tx *dbr.Tx) error {
	_, err := tx.InsertInto("group").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// Insert 插入群信息
func (d *DB) Insert(m *Model) error {
	_, err := d.session.InsertInto("group").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// 修改群类型
func (d *DB) UpdateGroupTypeTx(groupNo string, groupType GroupType, tx *dbr.Tx) error {
	_, err := tx.Update("group").Set("group_type", int(groupType)).Where("group_no=?", groupNo).Exec()
	return err
}

// 修改群类型
func (d *DB) UpdateGroupType(groupNo string, groupType GroupType) error {
	_, err := d.session.Update("group").Set("group_type", int(groupType)).Where("group_no=?", groupNo).Exec()
	return err
}

// InsertMemberTx 插入群成员信息(带事务)
func (d *DB) InsertMemberTx(m *MemberModel, tx *dbr.Tx) error {
	_, err := tx.InsertInto("group_member").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// InsertMember 插入群成员信息
func (d *DB) InsertMember(m *MemberModel) error {
	_, err := d.session.InsertInto("group_member").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// DeleteMemberTx 删除群成员
func (d *DB) DeleteMemberTx(groupNo string, uid string, version int64, tx *dbr.Tx) error {
	_, err := tx.Update("group_member").Set("is_deleted", 1).Set("version", version).Where("group_no=? and uid=?", groupNo, uid).Exec()
	return err
}

// DeleteMember 删除群成员
func (d *DB) DeleteMember(groupNo string, uid string, version int64) error {
	_, err := d.session.Update("group_member").Set("is_deleted", 1).Set("version", version).Where("group_no=? and uid=?", groupNo, uid).Exec()
	return err
}

// 真实删除群成员
func (d *DB) deleteMembersWithGroupNOTx(groupNo string, tx *dbr.Tx) error {
	_, err := tx.DeleteFrom("group_member").Where("group_no=?", groupNo).Exec()
	return err
}

// QuerySecondOldestMember 查询群里第二长老
func (d *DB) QuerySecondOldestMember(groupNo string) (*MemberModel, error) {
	var memberModel *MemberModel
	_, err := d.session.Select("*").From("group_member").Where("group_no=? and role<>? and is_deleted=0", groupNo, MemberRoleCreator).OrderDir("created_at", true).Load(&memberModel)
	return memberModel, err
}

// 通过vercode查询某个群成员
func (d *DB) queryMemberWithVercode(vercode string) (*MemberModel, error) {
	var memberModel *MemberModel
	_, err := d.session.Select("*").From("group_member").Where("vercode=?", vercode).Load(&memberModel)
	return memberModel, err
}

// 通过vercode查询某个群成员
func (d *DB) queryMemberWithVercodes(vercodes []string) ([]*MemberGroupDetailModel, error) {
	var memberModels []*MemberGroupDetailModel
	_, err := d.session.Select("group_member.*,IFNULL(`group`.name,'') group_name").From("group_member").LeftJoin("group", "`group`.group_no=group_member.group_no").Where("group_member.vercode in ?", vercodes).Load(&memberModels)
	return memberModels, err
}

// QueryIsGroupManagerOrCreator 是否是群管理者或创建者
//
// Fail-safe 过滤：
//   - is_external=0：外部成员即使在 DB 中残留 role=creator/manager（历史脏数据或
//     绕过 managerAdd 入口校验写入），也不视为该群的管理者。与 managerAdd /
//     transferGrouper 的前置 is_external 校验构成双层防御（YUJ-231 / GH#1289，
//     ReviewBot YUJ-230 P1）。
//   - status=GroupMemberStatusNormal：黑名单 / 已退出但 is_deleted 仍为 0 的成员
//     即便保留 role=creator/manager，也不视为有效管理者，避免被踢出的管理者继续
//     调用敏感 API（PR #31 round-3，Jerry-Xin）。
func (d *DB) QueryIsGroupManagerOrCreator(groupNo string, uid string) (bool, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").Where("group_no=? and uid=? and is_deleted=0 and is_external=0 and status=? and (role=? or role=?)", groupNo, uid, int(common.GroupMemberStatusNormal), MemberRoleCreator, MemberRoleManager).Load(&count)
	return count > 0, err
}

// QueryIsGroupCreator 是否是群创建者
func (d *DB) QueryIsGroupCreator(groupNo string, uid string) (bool, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").Where("group_no=? and uid=? and is_deleted=0 and role=?", groupNo, uid, MemberRoleCreator).Load(&count)
	return count > 0, err
}

// QueryGroupManagerOrCreatorUIDS 查询管理者或创建者的uid
func (d *DB) QueryGroupManagerOrCreatorUIDS(groupNo string) ([]string, error) {
	var uids []string
	_, err := d.session.Select("uid").From("group_member").Where("group_no=? and is_deleted=0 and (role=? or role=?)", groupNo, MemberRoleCreator, MemberRoleManager).Load(&uids)
	return uids, err
}

func (d *DB) queryGroupMemberMaxVersion(groupNo string) (int64, error) {
	var version int64
	_, err := d.session.Select("IFNULL(max(version),0)").From("group_member").Where("group_no=?", groupNo).Load(&version)
	return version, err
}

// UpdateMemberRoleTx 更新群成员角色
func (d *DB) UpdateMemberRoleTx(groupNo string, uid string, role int, version int64, tx *dbr.Tx) error {
	_, err := tx.Update("group_member").Set("role", role).Set("version", version).Where("group_no=? and uid=? and is_deleted=0", groupNo, uid).Exec()
	return err
}

// updateMemberForbiddenExpirTimeTx 修改成员禁言时长
func (d *DB) updateMemberForbiddenExpirTimeTx(groupNo string, uid string, time int, version int64, tx *dbr.Tx) error {
	_, err := tx.Update("group_member").Set("forbidden_expir_time", time).Set("version", version).Where("group_no=? and uid=? and is_deleted=0", groupNo, uid).Exec()
	return err
}

// UpdateMembersToManager 更新指定群成员为管理员
func (d *DB) UpdateMembersToManager(groupNo string, members []string, version int64) error {
	if len(members) <= 0 {
		return nil
	}
	_, err := d.session.Update("group_member").Set("role", MemberRoleManager).Set("version", version).Where("group_no=? and uid in ? and is_deleted=0", groupNo, members).Exec()
	return err
}

// UpdateManagersToMember 更新指定管理员为普通成员
func (d *DB) UpdateManagersToMember(groupNo string, members []string, version int64) error {
	if len(members) <= 0 {
		return nil
	}
	_, err := d.session.Update("group_member").Set("role", MemberRoleCommon).Set("version", version).Where("group_no=? and uid in ? and is_deleted=0", groupNo, members).Exec()
	return err
}

// ExistMember 群成员是否在群内
func (d *DB) ExistMember(uid string, groupNo string) (bool, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").Where("group_no=? and uid=? and is_deleted=0", groupNo, uid).Load(&count)
	return count > 0, err
}

// ExistMemberActive 群成员是否在群内且处于正常状态（白名单语义，fail closed）。
// 与 ExistMember 的差别：额外要求 status=GroupMemberStatusNormal，明确排除
// Blacklist 以及未来可能新增的非正常状态。
// 用于绕过 IM 层（直接读本地分表）的接口，避免被拉黑用户通过本地直查
// 拿到本应被 IM datasource 拦截的消息。
func (d *DB) ExistMemberActive(uid string, groupNo string) (bool, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").
		Where("group_no=? and uid=? and is_deleted=0 and status=?",
			groupNo, uid, common.GroupMemberStatusNormal).Load(&count)
	return count > 0, err
}

func (d *DB) existMembers(groupNos []string, uid string) ([]string, error) {
	var results []string
	_, err := d.session.Select("group_no").From("group_member").Where("group_no in ? and uid=? and is_deleted=0", groupNos, uid).Load(&results)
	return results, err
}

// ExistMemberDelete 存在已删除的群成员数据
func (d *DB) ExistMemberDelete(uid string, groupNo string) (bool, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").Where("group_no=? and uid=? and is_deleted=1", groupNo, uid).Load(&count)
	return count > 0, err
}

// UpdateMemberTx 更新成员信息
func (d *DB) UpdateMemberTx(member *MemberModel, tx *dbr.Tx) error {
	_, err := tx.Update("group_member").SetMap(map[string]interface{}{
		"remark":     member.Remark,
		"role":       member.Role,
		"version":    member.Version,
		"is_deleted": member.IsDeleted,
		"invite_uid": member.InviteUID,
	}).Where("group_no=? and uid=?", member.GroupNo, member.UID).Exec()
	return err
}

// recoverMemberTx 恢复成员信息
func (d *DB) recoverMemberTx(member *MemberModel, tx *dbr.Tx) error {
	_, err := tx.Update("group_member").SetMap(map[string]interface{}{
		"remark":          member.Remark,
		"role":            member.Role,
		"version":         member.Version,
		"is_deleted":      0,
		"invite_uid":      member.InviteUID,
		"is_external":     member.IsExternal,
		"source_space_id": member.SourceSpaceID,
		"created_at":      dbr.Expr("Now()"),
	}).Where("group_no=? and uid=?", member.GroupNo, member.UID).Exec()
	return err
}

// UpdateMember 更新群成员
func (d *DB) UpdateMember(member *MemberModel) error {
	_, err := d.session.Update("group_member").SetMap(map[string]interface{}{
		"remark":               member.Remark,
		"role":                 member.Role,
		"version":              member.Version,
		"is_deleted":           member.IsDeleted,
		"invite_uid":           member.InviteUID,
		"forbidden_expir_time": member.ForbiddenExpirTime,
	}).Where("group_no=? and uid=?", member.GroupNo, member.UID).Exec()
	return err
}

// 修改群成员状态
func (d *DB) updateMembersStatus(version int64, groupNo string, status int, uids []string) error {
	_, err := d.session.Update("group_member").SetMap(map[string]interface{}{
		"status":  status,
		"version": version,
	}).Where("group_no=? and uid in ?", groupNo, uids).Exec()
	return err
}

// QueryWithGroupNo 根据群编号查询群信息
func (d *DB) QueryWithGroupNo(groupNo string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("`group`").Where("group_no=?", groupNo).Load(&model)
	return model, err
}

// QueryWithGroupNo 根据群编号查询群信息
func (d *DB) QueryWithGroupNos(groupNos []string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("`group`").Where("group_no in ?", groupNos).Load(&models)
	return models, err
}

func (d *DB) queryUserSupers(uid string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("`group`.*").From("group_member").LeftJoin("group", "group.group_no=group_member.group_no").Where("group.group_type=? and group.status=? and group_member.is_deleted=0 and group_member.uid=?", GroupTypeSuper, GroupStatusNormal, uid).Load(&models)
	return models, err
}

// UpdateTx 更新群信息（带事务）
func (d *DB) UpdateTx(model *Model, tx *dbr.Tx) error {
	_, err := tx.Update("group").SetMap(map[string]interface{}{
		"name":      model.Name,
		"notice":    model.Notice,
		"creator":   model.Creator,
		"status":    model.Status,
		"version":   model.Version,
		"forbidden": model.Forbidden,
		"invite":    model.Invite,
	}).Where("id=?", model.Id).Exec()
	return err
}

// Update 更新群信息
func (d *DB) Update(model *Model) error {
	_, err := d.session.Update("group").SetMap(map[string]interface{}{
		"name":                        model.Name,
		"notice":                      model.Notice,
		"creator":                     model.Creator,
		"status":                      model.Status,
		"version":                     model.Version,
		"forbidden":                   model.Forbidden,
		"invite":                      model.Invite,
		"forbidden_add_friend":        model.ForbiddenAddFriend,
		"allow_view_history_msg":      model.AllowViewHistoryMsg,
		"allow_member_pinned_message": model.AllowMemberPinnedMessage,
		"allow_external":              model.AllowExternal,
		"allow_no_mention":            model.AllowNoMention,
	}).Where("id=?", model.Id).Exec()
	return err
}

func (d *DB) updateAvatar(avatar string, avatarVersion int64, groupNo string) error {
	_, err := d.session.Update("group").SetMap(map[string]interface{}{
		"avatar":           avatar,
		"avatar_version":   avatarVersion,
		"is_upload_avatar": 1,
	}).Where("group_no=?", groupNo).Exec()
	return err
}

// QueryDetailWithGroupNo 查询群详情
func (d *DB) QueryDetailWithGroupNo(groupNo string, uid string) (*DetailModel, error) {
	var detailModel *DetailModel
	_, err := d.session.Select("`group`.*,IFNULL(group_setting.version,0) + `group`.version  version,IFNULL(group_setting.chat_pwd_on,0) chat_pwd_on,IFNULL(group_setting.mute,0) mute,IFNULL(group_setting.top,0) top,IFNULL(group_setting.show_nick,0) show_nick,IFNULL(group_setting.save,0) save,IFNULL(group_setting.revoke_remind,1) revoke_remind,IFNULL(group_setting.join_group_remind,0) join_group_remind,IFNULL(group_setting.screenshot,1) screenshot,IFNULL(group_setting.receipt,1) receipt,IFNULL(group_setting.flame,0) flame,IFNULL(group_setting.flame_second,0) flame_second,IFNULL(group_setting.remark,'') remark").From("`group`").LeftJoin(`group_setting`, "`group`.group_no=group_setting.group_no and group_setting.uid=?").Where("`group`.group_no=?", uid, groupNo).Load(&detailModel)
	return detailModel, err
}

// QueryDetailWithGroupNos 查询群集合
func (d *DB) QueryDetailWithGroupNos(groupNos []string, uid string) ([]*DetailModel, error) {
	if len(groupNos) <= 0 {
		return nil, nil
	}
	var detailModels []*DetailModel
	_, err := d.session.Select("`group`.*,IFNULL(group_setting.version,0) + `group`.version  version,IFNULL(group_setting.chat_pwd_on,0) chat_pwd_on,IFNULL(group_setting.mute,0) mute,IFNULL(group_setting.top,0) top,IFNULL(group_setting.show_nick,0) show_nick,IFNULL(group_setting.save,0) save,IFNULL(group_setting.revoke_remind,1) revoke_remind,IFNULL(group_setting.join_group_remind,0) join_group_remind,IFNULL(group_setting.screenshot,1) screenshot,IFNULL(group_setting.receipt,1) receipt,IFNULL(group_setting.flame,0) flame,IFNULL(group_setting.flame_second,0) flame_second,IFNULL(group_setting.remark,'') remark").From("`group`").LeftJoin(`group_setting`, "`group`.group_no=group_setting.group_no and group_setting.uid=?").Where("`group`.group_no in ?", uid, groupNos).Load(&detailModels)
	return detailModels, err
}

// QueryGroupsWithGroupNos 通过群ID查询一批群信息
func (d *DB) QueryGroupsWithGroupNos(groupNos []string) ([]*Model, error) {
	if len(groupNos) <= 0 {
		return nil, nil
	}
	var models []*Model
	_, err := d.session.Select("*").From("`group`").Where("group_no in ?", groupNos).Load(&models)
	return models, err
}

// QueryMemberWithUID 查询群成员
func (d *DB) QueryMemberWithUID(uid string, groupNo string) (*MemberModel, error) {
	var memberModel *MemberModel
	_, err := d.session.Select("*").From("group_member").Where("uid=? and group_no=? and is_deleted=0", uid, groupNo).Load(&memberModel)
	return memberModel, err
}

// QueryMembersWithUids 查询群内的指定成员
func (d *DB) QueryMembersWithUids(uids []string, groupNo string) ([]*MemberModel, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	var memberModels []*MemberModel
	_, err := d.session.Select("*").From("group_member").Where("uid in ? and group_no=? and is_deleted=0", uids, groupNo).Load(&memberModels)
	return memberModels, err
}

// QueryMembersWithStatus 通过成员状态查询成员
func (d *DB) QueryMembersWithStatus(groupNo string, status int) ([]*MemberModel, error) {
	var memberModels []*MemberModel
	_, err := d.session.Select("*").From("group_member").Where("group_no=? and status=?", groupNo, status).Load(&memberModels)
	return memberModels, err
}

// QueryMemberWithUIDAndGroupNos
func (d *DB) QueryMemberWithUIDAndGroupNos(uid string, groupNos []string) ([]*MemberModel, error) {
	var memberModels []*MemberModel
	_, err := d.session.Select("*").From("group_member").Where("uid=? and group_no in ? and is_deleted=0", uid, groupNos).Load(&memberModels)
	return memberModels, err
}

// SyncMembers 同步群成员
func (d *DB) SyncMembers(groupNo string, version int64, limit uint64) ([]*MemberDetailModel, error) {

	var details []*MemberDetailModel
	builder := d.session.Select("group_member.id,group_member.vercode,group_member.uid,group_member.status,group_member.group_no,group_member.remark,group_member.role,IFNULL(user.name,'') name,IFNULL(user.username,'') username,group_member.is_deleted,group_member.robot,group_member.version,group_member.invite_uid,group_member.forbidden_expir_time,group_member.bot_admin,group_member.is_external,group_member.source_space_id,group_member.created_at,group_member.updated_at").From("group_member").LeftJoin("user", "group_member.uid=user.uid").Where("group_member.group_no=?", groupNo).OrderDir("group_member.version", true)
	var err error
	if version <= 0 {
		_, err = builder.Limit(limit).Load(&details)
	} else {
		_, err = builder.Where("group_member.version > ?", version).Limit(limit).Load(&details)
	}

	return details, err
}

// 通过名字关键字查询成员列表
func (d *DB) queryMembersWithKeyword(groupNo string, loginUID string, keyword string, page uint64, limit uint64) ([]*MemberDetailModel, error) {
	var details []*MemberDetailModel
	var builder *dbr.SelectStmt
	if keyword != "" {
		builder = d.session.Select("group_member.id,group_member.vercode,group_member.uid,group_member.status,group_member.group_no,group_member.remark,group_member.role,IFNULL(user.name,'') name,IFNULL(user.username,'') username,group_member.is_deleted,group_member.robot,group_member.version,group_member.invite_uid,group_member.forbidden_expir_time,group_member.bot_admin,group_member.is_external,group_member.source_space_id,group_member.created_at,group_member.updated_at").From("group_member").LeftJoin("user", "group_member.uid=user.uid").LeftJoin("user_setting", dbr.Expr("user_setting.uid=? and user_setting.to_uid=group_member.uid", loginUID)).Where("group_member.group_no=? and group_member.is_deleted=0 and group_member.status=1 and (group_member.remark like ? or user.name like ? or user_setting.remark like ?)", groupNo, "%"+keyword+"%", "%"+keyword+"%", "%"+keyword+"%").OrderAsc("group_member.created_at")
	} else {
		builder = d.session.Select("group_member.id,group_member.vercode,group_member.uid,group_member.status,group_member.group_no,group_member.remark,group_member.role,IFNULL(user.name,'') name,IFNULL(user.username,'') username,group_member.is_deleted,group_member.robot,group_member.version,group_member.invite_uid,group_member.forbidden_expir_time,group_member.bot_admin,group_member.is_external,group_member.source_space_id,group_member.created_at,group_member.updated_at").From("group_member").LeftJoin("user", "group_member.uid=user.uid").Where("group_member.group_no=? and group_member.is_deleted=0 and group_member.status=1", groupNo).OrderDesc(fmt.Sprintf("group_member.role=%d", MemberRoleCreator)).OrderDesc(fmt.Sprintf("group_member.role=%d", MemberRoleManager)).OrderAsc("group_member.created_at")
	}
	var err error
	_, err = builder.Offset((page - 1) * limit).Limit(limit).Load(&details)

	return details, err
}

func (d *DB) queryManagersWithGroupNos(groupNos []string) ([]*MemberDetailModel, error) {
	var memberModels []*MemberDetailModel
	_, err := d.session.Select("group_member.id,group_member.vercode,group_member.uid,group_member.status,group_member.group_no,group_member.remark,group_member.role,IFNULL(user.name,'') name,group_member.is_deleted,group_member.version,group_member.created_at,group_member.updated_at").From("group_member").LeftJoin("user", "group_member.uid=user.uid").Where("group_member.group_no in ? and group_member.is_deleted=0 and group_member.role<>0", groupNos).Load(&memberModels)
	return memberModels, err
}

func (d *DB) queryMembersWithGroupNo(groupNo string) ([]*MemberDetailModel, error) {
	var details []*MemberDetailModel
	_, err := d.session.Select("group_member.id,group_member.vercode,group_member.uid,group_member.status,group_member.group_no,group_member.remark,group_member.role,IFNULL(user.name,'') name,group_member.is_deleted,group_member.version,group_member.forbidden_expir_time,group_member.bot_admin,group_member.is_external,group_member.source_space_id,group_member.created_at,group_member.updated_at").From("group_member").LeftJoin("user", "group_member.uid=user.uid").Where("group_member.group_no=? and group_member.is_deleted=0", groupNo).Load(&details)
	return details, err
}

func (d *DB) queryMemberWithGroupNoAndUID(groupNo, uid string) (*MemberDetailModel, error) {
	var detail *MemberDetailModel
	_, err := d.session.Select("group_member.id,group_member.vercode,group_member.uid,group_member.status,group_member.group_no,group_member.remark,group_member.role,group_member.invite_uid,IFNULL(user.name,'') name,group_member.is_deleted,group_member.version,group_member.forbidden_expir_time,group_member.bot_admin,group_member.is_external,group_member.source_space_id,group_member.created_at,group_member.updated_at").From("group_member").LeftJoin("user", "group_member.uid=user.uid").Where("group_member.group_no=? and group_member.uid=? and group_member.is_deleted=0", groupNo, uid).Load(&detail)
	return detail, err
}
func (d *DB) queryBlacklistMemberUIDsWithGroupNo(groupNo string) ([]string, error) {
	var uids []string
	_, err := d.session.Select("group_member.uid").From("group_member").Where("group_member.group_no=? and group_member.is_deleted=0 and status=?", groupNo, common.GroupMemberStatusBlacklist).Load(&uids)
	return uids, err
}

// 查询在线成员数量
func (d *DB) queryMemberOnlineCount(groupNo string) (int64, error) {
	var count int64
	_, err := d.session.Select("count(DISTINCT user_online.uid)").From("group_member").LeftJoin("user_online", "group_member.uid=user_online.uid").Where("group_no=? and group_member.is_deleted=0 and user_online.online=1", groupNo).Load(&count)
	return count, err
}

// QueryMembersFirstNine 查询最先加入群聊的九为群成员
func (d *DB) QueryMembersFirstNine(groupNo string) ([]*MemberModel, error) {
	var memberModels []*MemberModel
	_, err := d.session.Select("*").From("group_member").Where("group_no=? and is_deleted=0", groupNo).OrderDir("created_at", true).Limit(9).Load(&memberModels)
	return memberModels, err
}

// QueryMembersFirstNineTx 事务内查询最先加入群聊的九位群成员
func (d *DB) QueryMembersFirstNineTx(groupNo string, tx *dbr.Tx) ([]*MemberModel, error) {
	var memberModels []*MemberModel
	_, err := tx.Select("*").From("group_member").Where("group_no=? and is_deleted=0", groupNo).OrderDir("created_at", true).Limit(9).Load(&memberModels)
	return memberModels, err
}

// QueryMembersFirstNineExclude 查询最先加入群聊的九位群成员 【excludeUIDs】为排除的用户
func (d *DB) QueryMembersFirstNineExclude(groupNo string, excludeUIDs []string) ([]*MemberModel, error) {
	if len(excludeUIDs) <= 0 {
		return d.QueryMembersFirstNine(groupNo)
	}
	var memberModels []*MemberModel
	_, err := d.session.Select("*").From("group_member").Where("group_no=? and is_deleted=0 and uid not in ?", groupNo, excludeUIDs).OrderDir("created_at", true).Limit(9).Load(&memberModels)
	return memberModels, err
}

// 成员是否在最先加入的9位成员内
func (d *DB) membersInFirstNine(groupNo string, uids []string) (bool, error) {
	if len(uids) == 0 {
		return false, nil
	}
	var count int
	err := d.session.SelectBySql("select count(*) from (select uid from group_member where group_no=? and is_deleted=0 order by created_at asc limit 9) t where t.uid in ?", groupNo, uids).LoadOne(&count)
	return count > 0, err
}

// QueryMemberCount 查询群成员数量
func (d *DB) QueryMemberCount(groupNo string) (int64, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").Where("group_no=? and is_deleted=0", groupNo).Load(&count)
	return count, err
}

// QueryMemberCountTx queries member count within a transaction using FOR UPDATE to prevent concurrent bypass.
func (d *DB) QueryMemberCountTx(groupNo string, tx *dbr.Tx) (int64, error) {
	var count int64
	_, err := tx.SelectBySql("SELECT count(*) FROM group_member WHERE group_no=? AND is_deleted=0 FOR UPDATE", groupNo).Load(&count)
	return count, err
}

// 查询群总数
func (d *DB) queryGroupCount() (int64, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("`group`").Load(&count)
	return count, err
}

// 查询某天的新建群数量
func (d *DB) queryCreatedCountWithDate(date string) (int64, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("`group`").Where("date_format(created_at,'%Y-%m-%d')=?", date).Load(&count)
	return count, err
}

// querySavedGroups 查询我保存的群
func (d *DB) querySavedGroups(uid string) ([]*DetailModel, error) {
	var detailModels []*DetailModel
	_, err := d.session.Select("`group`.*,IFNULL(group_setting.version,0) + `group`.version  version,IFNULL(group_setting.chat_pwd_on,0) chat_pwd_on,IFNULL(group_setting.mute,0) mute,IFNULL(group_setting.top,0) top,IFNULL(group_setting.show_nick,0) show_nick,IFNULL(group_setting.save,0) save,IFNULL(group_setting.remark,'') remark").From("`group`").LeftJoin(`group_setting`, "`group`.group_no=group_setting.group_no").Where("`group_setting`.save=1 and `group_setting`.uid=?", uid).Load(&detailModels)
	return detailModels, err
}

// queryGroupsWithMemberUIDAndSpaceID 查询某用户在某 Space 下加入的所有群
func (d *DB) queryGroupsWithMemberUIDAndSpaceID(memberUID string, spaceID string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("distinct `group`.*").From("`group`").LeftJoin("group_member", "`group`.group_no=group_member.group_no").Where("group_member.uid=? and group_member.is_deleted=0 and `group`.space_id=?", memberUID, spaceID).Load(&models)
	return models, err
}

// 查询某个用户参与的所有群
func (d *DB) queryGroupsWithMemberUID(memberUID string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("distinct `group`.*").From("`group`").LeftJoin("group_member", "`group`.group_no=group_member.group_no").Where("group_member.uid=? and group_member.is_deleted=0", memberUID).Load(&models)
	return models, err
}

// 查询禁言时长到期成员
func (d *DB) queryForbiddenExpirationTimeMembers(limit int64) ([]*MemberModel, error) {
	var models []*MemberModel
	_, err := d.session.Select("*").From("group_member").Where("forbidden_expir_time <>0 and unix_timestamp(now())-forbidden_expir_time>0").Limit(uint64(limit)).Load(&models)
	return models, err
}

// 查询群头像是否已被群主更新过
func (d *DB) queryGroupAvatarIsUpload(groupNo string) (int, error) {
	var result int
	err := d.session.Select("is_upload_avatar").From("`group`").Where("group_no=?", groupNo).LoadOne(&result)
	return result, err
}

// 查询用户当天建群数量
func (d *DB) querySameDayCreateCountWitUID(uid string, day string) (int, error) {
	var count int
	err := d.session.SelectBySql("SELECT COUNT(*) AS count FROM `group` WHERE creator=? AND DATE(created_at)=?", uid, day).LoadOne(&count)
	return count, err
}

// ---------- model ----------

// DetailModel 群详情
type DetailModel struct {
	Model
	Mute            int    // 免打扰
	Top             int    // 置顶
	ShowNick        int    // 显示昵称
	Save            int    // 是否保存
	ChatPwdOn       int    //是否开启聊天密码
	RevokeRemind    int    //撤回提醒
	JoinGroupRemind int    // 进群提醒
	Screenshot      int    //截屏通知
	Receipt         int    //消息是否回执
	Flame           int    // 是否开启阅后即焚
	FlameSecond     int    // 阅后即焚秒数
	Remark          string // 群备注
}

// Model 群db model
type Model struct {
	GroupNo                  string     // 群编号
	GroupType                int        // 群类型 0.普通群 1.超大群
	Name                     string     // 群名称
	Avatar                   string     // 群头像
	AvatarVersion            int64      // 群头像对象版本，0 表示旧版稳定路径
	IsUploadAvatar           int        // 群头像是否已经被用户上传
	Notice                   string     // 群公告
	Creator                  string     // 创建者uid
	Status                   int        // 群状态
	Version                  int64      // 版本号
	Forbidden                int        // 是否全员禁言
	Invite                   int        // 是否开启邀请确认 0.否 1.是
	ForbiddenAddFriend       int        //群内禁止加好友
	AllowViewHistoryMsg      int        // 是否允许新成员查看历史消息
	AllowMemberPinnedMessage int        // 是否允许群成员置顶消息
	Category                 string     // 群分类
	SpaceID                  string     // Space ID
	IsExternalGroup          int        // 外部群 0.否 1.是（自动维护）
	AllowExternal            int        // 是否允许外部成员加入 1.允许(默认) 0.禁止
	AllowNoMention           int        // 群级是否允许免@生效 1.允许(默认) 0.禁止（bot 在本群必须被@）
	GroupMd                  *string    // GROUP.md content
	GroupMdVersion           int64      // GROUP.md version
	GroupMdUpdatedAt         *time.Time // GROUP.md last update time
	GroupMdUpdatedBy         string     // GROUP.md last updater UID
	db.BaseModel
}

// MemberModel 成员model
type MemberModel struct {
	GroupNo            string // 群编号
	UID                string // 成员uid
	Remark             string // 成员备注
	Role               int    // 成员角色 1. 创建者	 2.管理员
	Version            int64
	Status             int    // 1.正常 2.黑名单
	Vercode            string //验证码
	IsDeleted          int    // 是否删除
	InviteUID          string // 邀请者
	Robot              int    // 机器人
	ForbiddenExpirTime int64  // 禁言时长
	IsExternal         int    // 外部成员 0.否 1.是
	SourceSpaceID      string // 来源 Space ID（外部成员使用）
	db.BaseModel
}

// MemberDetailModel 成员详情model
type MemberDetailModel struct {
	UID                string // 成员uid
	GroupNo            string // 群编号
	Name               string // 群成员名称
	Remark             string // 成员备注
	Role               int    // 成员角色
	Version            int64
	Vercode            string //验证码
	InviteUID          string // 邀请人
	IsDeleted          int    // 是否删除
	Status             int    // 1.正常 2.黑名单
	Username           string
	Robot              int    // 机器人标识0.否1.是
	ForbiddenExpirTime int64  // 禁言时长
	BotAdmin           int    // Bot管理员 0.否 1.是
	IsExternal         int    // 外部成员 0.否 1.是
	SourceSpaceID      string // 来源 Space ID（外部成员使用）
	db.BaseModel
}

type MemberGroupDetailModel struct {
	GroupName string // 群名称
	MemberModel
}

// GroupMdResult GROUP.md query result
type GroupMdResult struct {
	Content   string     `json:"content"`
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at"`
	UpdatedBy string     `json:"updated_by"`
}

// QueryGroupMd queries GROUP.md content for a group
func (d *DB) QueryGroupMd(groupNo string) (*GroupMdResult, error) {
	var result *GroupMdResult
	_, err := d.session.Select("IFNULL(group_md,'') as content, group_md_version as version, group_md_updated_at as updated_at, group_md_updated_by as updated_by").From("`group`").Where("group_no=?", groupNo).Load(&result)
	return result, err
}

// UpdateGroupMd updates GROUP.md content and auto-increments version.
// Uses a transaction to ensure UPDATE and SELECT LAST_INSERT_ID() share the same connection.
func (d *DB) UpdateGroupMd(groupNo string, content string, updatedBy string) (int64, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.RollbackUnlessCommitted()

	_, err = tx.UpdateBySql(
		"UPDATE `group` SET group_md=?, group_md_version=LAST_INSERT_ID(group_md_version+1), group_md_updated_at=NOW(), group_md_updated_by=? WHERE group_no=?",
		content, updatedBy, groupNo,
	).Exec()
	if err != nil {
		return 0, err
	}
	var newVersion int64
	_, err = tx.SelectBySql("SELECT LAST_INSERT_ID()").Load(&newVersion)
	if err != nil {
		return 0, err
	}
	return newVersion, tx.Commit()
}

// DeleteGroupMd sets group_md=NULL and increments version.
// Uses a transaction to ensure UPDATE and SELECT LAST_INSERT_ID() share the same connection.
func (d *DB) DeleteGroupMd(groupNo string) (int64, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.RollbackUnlessCommitted()

	_, err = tx.UpdateBySql(
		"UPDATE `group` SET group_md=NULL, group_md_version=LAST_INSERT_ID(group_md_version+1), group_md_updated_at=NOW(), group_md_updated_by='' WHERE group_no=?",
		groupNo,
	).Exec()
	if err != nil {
		return 0, err
	}
	var newVersion int64
	_, err = tx.SelectBySql("SELECT LAST_INSERT_ID()").Load(&newVersion)
	if err != nil {
		return 0, err
	}
	return newVersion, tx.Commit()
}

// QueryIsBotAdmin checks if a member is a bot admin in the group
func (d *DB) QueryIsBotAdmin(groupNo string, uid string) (bool, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("group_member").Where("group_no=? and uid=? and is_deleted=0 and bot_admin=1", groupNo, uid).Load(&count)
	return count > 0, err
}

// UpdateBotAdmin sets or unsets bot_admin for a group member
func (d *DB) UpdateBotAdmin(groupNo string, uid string, isBotAdmin int, version int64) error {
	_, err := d.session.Update("group_member").Set("bot_admin", isBotAdmin).Set("version", version).Where("group_no=? and uid=? and is_deleted=0", groupNo, uid).Exec()
	return err
}

// QueryBotMemberUIDs returns UIDs of robot=1 members in the group
func (d *DB) QueryBotMemberUIDs(groupNo string) ([]string, error) {
	var uids []string
	_, err := d.session.Select("uid").From("group_member").Where("group_no=? and is_deleted=0 and robot=1", groupNo).Load(&uids)
	return uids, err
}

// QueryBotsInvitedByUIDTx 事务内查询群里由 inviterUID 所拥有的活跃 bot 成员 UID 列表，带 FOR UPDATE 行锁。
//
// D-2 cascade 语义（YUJ-49 / Mininglamp-OSS/octo-server#1186）：
//   - Bot 进群本质是邀请关系延伸（#1182 已强制 inviter == robot.creator_uid）
//   - inviter 离群（主动退 / 被踢）时，应同事务级联移除其所有 bot
//
// 只返回同时满足以下条件的 bot UID：
//   - group_member.robot = 1 AND is_deleted = 0（仍在群内的 bot 成员）
//   - robot.creator_uid = inviterUID AND robot.status = 1（仍活跃、且属于 inviter）
//
// 为什么是 INNER JOIN + status=1：与 checkBotOwnership（bot_ownership.go）保持一致，
// 没有活跃 robot 行的 bot（孤儿 / 禁用）不视为任何人的 bot，不被级联。
// 群主 / 其他管理员仍可通过常规移除成员接口清理它们。
func (d *DB) QueryBotsInvitedByUIDTx(groupNo string, inviterUID string, tx *dbr.Tx) ([]string, error) {
	if groupNo == "" || inviterUID == "" {
		return nil, nil
	}
	var uids []string
	_, err := tx.SelectBySql(
		"SELECT gm.uid FROM group_member gm "+
			"INNER JOIN robot r ON r.robot_id = gm.uid AND r.status = 1 "+
			"WHERE gm.group_no = ? AND gm.robot = 1 AND gm.is_deleted = 0 "+
			"AND r.creator_uid = ? FOR UPDATE",
		groupNo, inviterUID,
	).Load(&uids)
	return uids, err
}

// QueryExternalMemberCountTx 事务内查询群内「人类」外部成员数量（FOR UPDATE 行锁防并发）。
// 排除 robot=1 的 bot 成员：is_external_group 的语义只反映人类外部成员是否存在，
// bot 的 is_external + source_space_id 字段仅用于能力路由，不影响群的外部属性。
// 详见 YUJ-48 / Mininglamp-OSS/octo-server#1184。
func (d *DB) QueryExternalMemberCountTx(groupNo string, tx *dbr.Tx) (int64, error) {
	var count int64
	_, err := tx.SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_external=1 AND is_deleted=0 AND robot=0 FOR UPDATE",
		groupNo,
	).Load(&count)
	return count, err
}

// QueryExternalGroupNosForUser 查询用户作为外部成员加入的群列表，返回 groupNo -> sourceSpaceID
func (d *DB) QueryExternalGroupNosForUser(uid string) (map[string]string, error) {
	result := make(map[string]string)
	if uid == "" {
		return result, nil
	}
	var rows []struct {
		GroupNo       string `db:"group_no"`
		SourceSpaceID string `db:"source_space_id"`
	}
	_, err := d.session.SelectBySql(
		"SELECT group_no, source_space_id FROM group_member WHERE uid=? AND is_external=1 AND is_deleted=0",
		uid,
	).Load(&rows)
	if err != nil {
		return result, err
	}
	for _, r := range rows {
		result[r.GroupNo] = r.SourceSpaceID
	}
	return result, nil
}

// UpdateIsExternalGroup 更新群的 is_external_group 标记
func (d *DB) UpdateIsExternalGroup(groupNo string, value int) error {
	_, err := d.session.Update("group").
		Set("is_external_group", value).
		Where("group_no=?", groupNo).Exec()
	return err
}

// memberExternalMarkerRow 是 queryMemberExternalMarkers 的内部扁平行结构。
type memberExternalMarkerRow struct {
	UID             string `db:"uid"`
	IsExternal      int    `db:"is_external"`
	SourceSpaceID   string `db:"source_space_id"`
	SourceSpaceName string `db:"source_space_name"`
}

// queryMemberExternalMarkers 一次性拉取群内所有未删除成员的 is_external/source_space_id/source_space_name，
// 供消息同步热路径 O(1) lookup。使用 LEFT JOIN space 以便即使来源 Space 不存在也不漏成员。
// source_space_id 透传给上层，用于计算 home_space_id（YUJ-63 / #1208）。
func (d *DB) queryMemberExternalMarkers(groupNo string) ([]*memberExternalMarkerRow, error) {
	var rows []*memberExternalMarkerRow
	_, err := d.session.SelectBySql(
		"SELECT gm.uid AS uid, gm.is_external AS is_external, "+
			"IFNULL(gm.source_space_id,'') AS source_space_id, "+
			"IFNULL(s.name,'') AS source_space_name "+
			"FROM group_member gm LEFT JOIN space s ON s.space_id = gm.source_space_id "+
			"WHERE gm.group_no = ? AND gm.is_deleted = 0",
		groupNo,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateIsExternalGroupTx 事务内更新群的 is_external_group 标记
func (d *DB) UpdateIsExternalGroupTx(groupNo string, value int, tx *dbr.Tx) error {
	_, err := tx.Update("group").
		Set("is_external_group", value).
		Where("group_no=?", groupNo).Exec()
	return err
}

// QuerySourceSpaceIDForMember 查询某用户作为外部成员在指定群的 source_space_id
// 非外部成员或不存在时返回空字符串
func (d *DB) QuerySourceSpaceIDForMember(groupNo, uid string) (string, error) {
	if groupNo == "" || uid == "" {
		return "", nil
	}
	var sourceSpaceID string
	err := d.session.SelectBySql(
		"SELECT source_space_id FROM group_member WHERE group_no=? AND uid=? AND is_external=1 AND is_deleted=0",
		groupNo, uid,
	).LoadOne(&sourceSpaceID)
	if err != nil && err != dbr.ErrNotFound {
		return "", err
	}
	return sourceSpaceID, nil
}

// queryMemberExternalMarker 单成员版本的 queryMemberExternalMarkers，供 /users/{uid}?group_no
// 路径使用。返回 nil, nil 表示 uid 不在群内 / 已删除。
//
// 单独抽函数而非复用 queryMemberExternalMarkers，因为群成员数可能达到上万，
// 为单点接口全量拉取代价远高于一条点查；LEFT JOIN 空间换时间完全一致。
func (d *DB) queryMemberExternalMarker(groupNo, uid string) (*memberExternalMarkerRow, error) {
	if strings.TrimSpace(groupNo) == "" || strings.TrimSpace(uid) == "" {
		return nil, nil
	}
	var row *memberExternalMarkerRow
	_, err := d.session.SelectBySql(
		"SELECT gm.uid AS uid, gm.is_external AS is_external, "+
			"IFNULL(gm.source_space_id,'') AS source_space_id, "+
			"IFNULL(s.name,'') AS source_space_name "+
			"FROM group_member gm LEFT JOIN space s ON s.space_id = gm.source_space_id "+
			"WHERE gm.group_no = ? AND gm.uid = ? AND gm.is_deleted = 0",
		groupNo, uid,
	).Load(&row)
	if err != nil {
		return nil, err
	}
	return row, nil
}

type CategoryRow struct {
	CategoryID string `db:"category_id"`
	UID        string `db:"uid"`
	SpaceID    string `db:"space_id"`
	Status     int    `db:"status"`
}

func (d *DB) QueryCategoryByID(categoryID string) (*CategoryRow, error) {
	var row *CategoryRow
	_, err := d.session.Select("category_id", "uid", "space_id", "status").
		From("group_category").Where("category_id=?", categoryID).Load(&row)
	return row, err
}
