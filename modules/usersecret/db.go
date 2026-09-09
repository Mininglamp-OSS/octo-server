package usersecret

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// secretStore 是 usersecret 数据访问的行为契约。service / API 都依赖它而非具体
// *store,让测试能注入「让某次查询报错」的 fault store,验证 resolve 的错误分类
// (DB/auth-query 故障 → internal_error,而非误记 not_found/decrypt_fail)真正被
// 守住(R3.2:回归测试必须能抓回原 classification bug,不能 mock-blind)。
// 接口方法未导出,故只能在本包内实现,不构成对外 API 面。
type secretStore interface {
	insertAlias(m *aliasModel) error
	queryBySecretID(ownerUID, secretID string) (*aliasModel, error)
	listByOwner(ownerUID string) ([]*aliasModel, error)
	updateSecret(ownerUID, secretID string, cipher []byte, masked string) (int64, error)
	renameAlias(ownerUID, secretID, displayName, norm string) (int64, error)
	deleteAlias(ownerUID, secretID string) (int64, error)
	touchLastUsed(ownerUID, secretID string) error
	insertResolveAudit(m *resolveAuditModel) error
	queryBotByToken(botToken string) (*botIdentity, error)
}

// 编译期断言:*store 必须实现 secretStore。
var _ secretStore = (*store)(nil)

// store 别名表数据访问层。
type store struct {
	session *dbr.Session
}

func newStore(ctx *config.Context) *store {
	return &store{session: ctx.DB()}
}

// insertAlias 新增别名。唯一键冲突由调用方通过 isDuplicateErr 区分。
func (s *store) insertAlias(m *aliasModel) error {
	if _, err := s.session.InsertInto("user_secret_alias").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec(); err != nil {
		return fmt.Errorf("usersecret: insert alias: %w", err)
	}
	return nil
}

// queryBySecretID 按 secret_id + owner 查询(owner 限定防越权)。未命中 (nil,nil)。
func (s *store) queryBySecretID(ownerUID, secretID string) (*aliasModel, error) {
	var m *aliasModel
	if _, err := s.session.Select("*").From("user_secret_alias").
		Where("owner_uid=? AND secret_id=?", ownerUID, secretID).
		Load(&m); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, fmt.Errorf("usersecret: query by secret_id: %w", err)
	}
	return m, nil
}

// listByOwner 列出某 owner 的全部别名(按创建时间倒序)。
func (s *store) listByOwner(ownerUID string) ([]*aliasModel, error) {
	var list []*aliasModel
	if _, err := s.session.Select("*").From("user_secret_alias").
		Where("owner_uid=?", ownerUID).
		OrderDir("created_at", false).
		Load(&list); err != nil {
		return nil, fmt.Errorf("usersecret: list by owner: %w", err)
	}
	return list, nil
}

// updateSecret 换 key:只更新密文 + 掩码,secret_id/display_name 不变。
// 返回受影响行数(0 表示未命中该 owner 的该 secret_id)。
func (s *store) updateSecret(ownerUID, secretID string, cipher []byte, masked string) (int64, error) {
	res, err := s.session.Update("user_secret_alias").
		SetMap(map[string]interface{}{
			"cipher_text": cipher,
			"masked":      masked,
		}).
		Where("owner_uid=? AND secret_id=?", ownerUID, secretID).Exec()
	if err != nil {
		return 0, fmt.Errorf("usersecret: update secret: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// renameAlias 重命名:更新 display_name + display_name_norm,密文不变。
// 唯一键冲突由调用方 isDuplicateErr 区分。返回受影响行数。
func (s *store) renameAlias(ownerUID, secretID, displayName, norm string) (int64, error) {
	res, err := s.session.Update("user_secret_alias").
		SetMap(map[string]interface{}{
			"display_name":      displayName,
			"display_name_norm": norm,
		}).
		Where("owner_uid=? AND secret_id=?", ownerUID, secretID).Exec()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// deleteAlias 删除别名。返回受影响行数(0 表示未命中)。
func (s *store) deleteAlias(ownerUID, secretID string) (int64, error) {
	res, err := s.session.DeleteFrom("user_secret_alias").
		Where("owner_uid=? AND secret_id=?", ownerUID, secretID).Exec()
	if err != nil {
		return 0, fmt.Errorf("usersecret: delete alias: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// touchLastUsed best-effort 回写 last_used_at(resolve 成功后调用)。
// 显式把 updated_at 设回自身,避开列上的 `on update CURRENT_TIMESTAMP`:
// 「最后使用」是读侧元数据,不应污染「最后修改」时间。
// owner_uid 一并限定:与其它 accessor 一致,defense-in-depth 防越权回写。
func (s *store) touchLastUsed(ownerUID, secretID string) error {
	_, err := s.session.Update("user_secret_alias").
		SetMap(map[string]interface{}{
			"last_used_at": time.Now(),
			"updated_at":   dbr.Expr("updated_at"),
		}).
		Where("owner_uid=? AND secret_id=?", ownerUID, secretID).Exec()
	return err
}

// insertResolveAudit 写一条 resolve 审计(best-effort,失败仅记日志)。
func (s *store) insertResolveAudit(m *resolveAuditModel) error {
	if _, err := s.session.InsertInto("user_secret_resolve_audit").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec(); err != nil {
		return fmt.Errorf("usersecret: insert resolve audit: %w", err)
	}
	return nil
}

// isDuplicateErr 判断是否 MySQL 唯一键冲突(1062),用于把 DB 层冲突翻译成
// 业务层「别名撞名」。与项目其他模块对 mysql.MySQLError.Number==1062 的判定一致。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}

// botIdentity resolve 鉴权解析出的调用方身份。
type botIdentity struct {
	RobotID  string
	OwnerUID string // robot.creator_uid —— key 的归属用户
}

// queryBotByToken 用 bf_ bot token 反查 robot,返回 robot_id + creator_uid(owner)。
//
// 仅认 status=1 且 bot_token 非空的 User Bot —— 与 bot_api.authUserBot 的口径一致。
// 未命中返回 (nil, nil),由调用方按「鉴权失败」处理。
func (s *store) queryBotByToken(botToken string) (*botIdentity, error) {
	if botToken == "" {
		return nil, nil
	}
	var row struct {
		RobotID    string
		CreatorUID string
	}
	found, err := s.session.Select("robot_id", "creator_uid").From("robot").
		Where("bot_token=? AND bot_token!='' AND status=1 AND kind='user' AND creator_uid!=''", botToken).
		Load(&row)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, fmt.Errorf("usersecret: query bot by token: %w", err)
	}
	if found == 0 || row.RobotID == "" {
		return nil, nil
	}
	return &botIdentity{RobotID: row.RobotID, OwnerUID: row.CreatorUID}, nil
}
