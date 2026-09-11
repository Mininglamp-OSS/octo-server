package user

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

// DB DB
type onlineDB struct {
	session *dbr.Session
	ctx     *config.Context
}

// newOnlineDB newOnlineDB
func newOnlineDB(ctx *config.Context) *onlineDB {
	return &onlineDB{
		session: ctx.DB(),
		ctx:     ctx,
	}
}

// insertOrUpdateUserOnlineTx 插入或更新用户在线信息
func (o *onlineDB) insertOrUpdateUserOnlineTx(m *onlineStatusModel, tx *dbr.Tx) error {
	var err error
	if m.Online == 1 {
		_, err = tx.UpdateBySql("insert into user_online (uid,device_flag,last_online,online,version) values(?,?,?,1,?) ON DUPLICATE KEY UPDATE last_online=VALUES(last_online),online=VALUES(online),updated_at=NOW(),version=VALUES(version)", m.UID, m.DeviceFlag, m.LastOnline, m.Version).Exec()
	} else {
		_, err = tx.UpdateBySql("insert into user_online (uid,device_flag,last_offline,online,version) values(?,?,?,0,?) ON DUPLICATE KEY UPDATE last_offline=VALUES(last_offline),online=VALUES(online),updated_at=NOW(),version=VALUES(version)", m.UID, m.DeviceFlag, m.LastOffline, m.Version).Exec()
	}

	return err
}

// queryUserOnlineRecets 查询最近在线的用户(最近是指一小时内在线的,最多查询到1000条)
func (o *onlineDB) queryUserOnlineRecets(uids []string) ([]*onlineStatusWeightModel, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	var models []*onlineStatusWeightModel
	_, err := o.session.Select("user_online.*,IFNULL(device_flag.weight,0) weight").From("user_online").LeftJoin("device_flag", "user_online.device_flag=device_flag.device_flag").Where("user_online.uid in ? and ( unix_timestamp(now())-user_online.last_offline<3600*24 or user_online.online=1)", uids).OrderDir("user_online.online", false).OrderDir("user_online.last_offline", false).Load(&models)
	onlineStatusMap := map[string]*onlineStatusWeightModel{}
	if len(models) > 0 {
		for _, m := range models {
			oldOnline := onlineStatusMap[m.UID]
			if oldOnline == nil {
				onlineStatusMap[m.UID] = m
				continue
			}
			replace := false
			if m.Online == 1 && oldOnline.Online == 0 {
				replace = true
			}
			if m.Online == 1 && oldOnline.Online == 1 && m.Weight > oldOnline.Weight {
				replace = true
			}
			if m.Online != 1 && oldOnline.Online != 1 && m.LastOffline > oldOnline.LastOffline {
				replace = true
			}
			if replace {
				onlineStatusMap[m.UID] = m
			}

		}
	}
	newModels := make([]*onlineStatusWeightModel, 0, len(onlineStatusMap))
	for _, value := range onlineStatusMap {
		newModels = append(newModels, value)
	}
	return newModels, err
}

func (o *onlineDB) queryUserLastNewOnlines(uids []string) ([]*onlineStatusWeightModel, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	var models []*onlineStatusWeightModel
	_, err := o.session.Select("user_online.*,IFNULL(device_flag.weight,0) weight").From("user_online").LeftJoin("device_flag", "user_online.device_flag=device_flag.device_flag").Where("user_online.uid in ?", uids).OrderDir("user_online.online", false).OrderDir("user_online.last_offline", false).Load(&models)
	onlineStatusMap := map[string]*onlineStatusWeightModel{}
	if len(models) > 0 {
		for _, m := range models {
			oldOnline := onlineStatusMap[m.UID]
			if oldOnline == nil {
				onlineStatusMap[m.UID] = m
				continue
			}
			replace := false
			if m.Online == 1 && oldOnline.Online == 0 {
				replace = true
			}
			if m.Online == 1 && oldOnline.Online == 1 && m.Weight > oldOnline.Weight {
				replace = true
			}
			if m.Online != 1 && oldOnline.Online != 1 && m.LastOffline > oldOnline.LastOffline {
				replace = true
			}
			if replace {
				onlineStatusMap[m.UID] = m
			}

		}
	}
	newModels := make([]*onlineStatusWeightModel, 0, len(onlineStatusMap))
	for _, value := range onlineStatusMap {
		newModels = append(newModels, value)
	}
	return newModels, err
}

func (o *onlineDB) queryOnlinesMoreThan(t time.Duration, limit uint64) ([]*onlineStatusModel, error) {
	var models []*onlineStatusModel
	_, err := o.session.Select("*").From("user_online").Where("`online`=1 and unix_timestamp(now()) - last_online>?", t.Seconds()).Limit(limit).Load(&models)
	return models, err
}

// 查询用户最近在线设备
func (o *onlineDB) queryLastOnlineDeviceWithUID(uid string) (*onlineStatusModel, error) {
	var model *onlineStatusModel
	_, err := o.session.Select("*").From("user_online").Where("uid=?", uid).OrderDesc("online=1").OrderDesc("last_offline").Limit(1).Load(&model)
	return model, err
}

func (o *onlineDB) queryOnlineDevice(uid string, deviceFlag config.DeviceFlag) (*onlineStatusModel, error) {
	var onlineStatusModel *onlineStatusModel
	_, err := o.session.Select("*").From("user_online").Where("uid=? and device_flag=?", uid, deviceFlag.Uint8()).Load(&onlineStatusModel)
	return onlineStatusModel, err
}

// queryOnlineDevices 批量查询给定 uid 集合中指定设备的在线记录（仅 online=1），
// 供离线 Push 前的批量在线判定使用，避免 per-uid 循环查询造成的 N+1。
//
// deviceFlags 必须用 uint16 而非 uint8：Go 里 []uint8 即 []byte，dbr 会把它当二进制字面量
// 而不是展开成 IN 列表，生成的 `device_flag in ?` 是非法 SQL（MySQL 1064），整条查询恒失败。
// uint16 元素类型才会正确展开成 `(1,2)`。
// uid 按 1000 分批，与相邻 user_notification_pause 的 getActiveByUIDs 保持一致，避免超大 IN。
func (o *onlineDB) queryOnlineDevices(uids []string, deviceFlags []uint16) ([]*onlineStatusModel, error) {
	if len(uids) == 0 || len(deviceFlags) == 0 {
		return nil, nil
	}
	var models []*onlineStatusModel
	const batchSize = 1000
	for start := 0; start < len(uids); start += batchSize {
		end := start + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		var batch []*onlineStatusModel
		_, err := o.session.Select("*").From("user_online").
			Where("uid in ? and device_flag in ? and `online`=1", uids[start:end], deviceFlags).Load(&batch)
		if err != nil {
			return nil, err
		}
		models = append(models, batch...)
	}
	return models, nil
}

func (o *onlineDB) exist(uid string, deviceFlag uint8, online int) (bool, error) {
	var cn int
	_, err := o.session.Select("count(*)").From("user_online").Where("uid=? and device_flag=? and `online`=?", uid, deviceFlag, online).Load(&cn)
	return cn > 0, err
}

// 查询用户在线设备里最大权重的在线状态
func (o *onlineDB) queryOnlineMaxWeightWithUID(uid string) (*onlineStatusModel, error) {
	var onlineStatusModel *onlineStatusModel
	_, err := o.session.Select("user_online.*").From("user_online").LeftJoin("device_flag", "user_online.device_flag=device_flag.device_flag").Where("uid=? and online=1", uid).OrderDesc("device_flag.weight").Limit(1).Load(&onlineStatusModel)
	return onlineStatusModel, err
}

// 查询在线用户总数量
func (o *onlineDB) queryOnlineCount() (int64, error) {
	var count int64
	_, err := o.session.SelectBySql("select count(distinct uid) as count from user_online where online=1").Load(&count)
	return count, err
}

// OnlineStatusModel 在线状态model
type onlineStatusModel struct {
	UID         string
	DeviceFlag  uint8 // 设备标记 0. APP 1.web
	LastOnline  int   // 最后一次在线时间
	LastOffline int   // 最后一次离线时间
	Online      int
	Version     int64 // 数据版本
	db.BaseModel
}

type onlineStatusWeightModel struct {
	onlineStatusModel
	Weight int // 设备权重
}
