package featuregate

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// 表名常量。Select/From 需要手写反引号（dbr 只对 InsertInto/Update/DeleteFrom
// 的表名自动 quote），故这里备两份，避免调用点各写各的拼错。
const (
	tableGate            = "octo_feature_gate"
	tableGateQuoted      = "`octo_feature_gate`"
	tableGateScope       = "octo_feature_gate_scope"
	tableGateScopeQuoted = "`octo_feature_gate_scope`"
)

// gateDB 是 octo_feature_gate / octo_feature_gate_scope 两张表的持久化层。
// DB 是规则的单一真源；Service 在其上叠加 Redis 读缓存。
type gateDB struct {
	session *dbr.Session
}

func newDB(ctx *config.Context) *gateDB {
	return &gateDB{session: ctx.DB()}
}

// queryRule 按 feature_key 取规则；不存在返回 (nil, nil)（Load 到 **struct，
// 无结果时指针保持 nil），调用方按 m == nil 判定未注册。
func (d *gateDB) queryRule(key string) (*gateModel, error) {
	var m *gateModel
	_, err := d.session.Select("*").From(tableGateQuoted).
		Where("feature_key=?", key).Load(&m)
	return m, err
}

// queryScopes 取某 key 的全部白名单条目。
func (d *gateDB) queryScopes(key string) ([]*scopeModel, error) {
	var list []*scopeModel
	_, err := d.session.Select("*").From(tableGateScopeQuoted).
		Where("feature_key=?", key).
		OrderDir("scope_id", true).Load(&list)
	return list, err
}

// listRules 列出所有规则，供管理端列表。
func (d *gateDB) listRules() ([]*gateModel, error) {
	var list []*gateModel
	_, err := d.session.Select("*").From(tableGateQuoted).
		OrderDir("feature_key", true).Load(&list)
	return list, err
}

// upsertRule 按 feature_key 写入/覆盖规则。靠唯一键 uk_octo_feature_gate_key 幂等：
// 不存在则创建（注册新功能 gate），存在则覆盖 mode/percent/bucket_by/description。
func (d *gateDB) upsertRule(key, mode string, percent int, bucketBy, description string) error {
	_, err := d.session.InsertBySql(
		"INSERT INTO "+tableGateQuoted+" (feature_key, mode, percent, bucket_by, description) "+
			"VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE mode=VALUES(mode), percent=VALUES(percent), "+
			"bucket_by=VALUES(bucket_by), description=VALUES(description)",
		key, mode, percent, bucketBy, description,
	).Exec()
	if err != nil {
		return fmt.Errorf("featuregate: upsert rule %q: %w", key, err)
	}
	return nil
}

// addScope 幂等地加入一条白名单条目；重复 (key,type,id) 命中唯一键走 no-op
// 更新，对调用方表现为成功。
func (d *gateDB) addScope(key, scopeType, scopeID string) error {
	_, err := d.session.InsertBySql(
		"INSERT INTO "+tableGateScopeQuoted+" (feature_key, scope_type, scope_id) "+
			"VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE feature_key=feature_key",
		key, scopeType, scopeID,
	).Exec()
	if err != nil {
		return fmt.Errorf("featuregate: add scope %q/%s/%s: %w", key, scopeType, scopeID, err)
	}
	return nil
}

// deleteScope 删除一条白名单条目，返回受影响行数（0 表示该条目不存在，调用方
// 据此回 404）。
func (d *gateDB) deleteScope(key, scopeType, scopeID string) (int64, error) {
	res, err := d.session.DeleteFrom(tableGateScope).
		Where("feature_key=? AND scope_type=? AND scope_id=?", key, scopeType, scopeID).Exec()
	if err != nil {
		return 0, fmt.Errorf("featuregate: delete scope %q/%s/%s: %w", key, scopeType, scopeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("featuregate: delete scope rows affected: %w", err)
	}
	return n, nil
}
