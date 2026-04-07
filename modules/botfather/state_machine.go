package botfather

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// stateMachine Redis Hash状态机，管理BotFather多轮对话状态
// key 格式：botfather:state:{uid}:{spaceID}（Space 隔离）
// 无 spaceID 时回退到 botfather:state:{uid}（向前兼容）
type stateMachine struct {
	ctx *config.Context
}

func newStateMachine(ctx *config.Context) *stateMachine {
	return &stateMachine{ctx: ctx}
}

func (sm *stateMachine) key(uid string, spaceID string) string {
	if spaceID != "" {
		return fmt.Sprintf("%s%s:%s", stateKeyPrefix, uid, spaceID)
	}
	return fmt.Sprintf("%s%s", stateKeyPrefix, uid)
}

// GetState 获取用户当前对话状态
func (sm *stateMachine) GetState(uid string, spaceID string) (string, error) {
	return sm.GetField(uid, spaceID, FieldState)
}

// GetField 获取状态中的某个字段
func (sm *stateMachine) GetField(uid string, spaceID string, field string) (string, error) {
	result, err := sm.ctx.GetRedisConn().Hget(sm.key(uid, spaceID), field)
	if err != nil {
		return "", err
	}
	return result, nil
}

// SetState 设置对话状态
func (sm *stateMachine) SetState(uid string, spaceID string, state string, command string) error {
	k := sm.key(uid, spaceID)
	err := sm.ctx.GetRedisConn().Hset(k, FieldState, state)
	if err != nil {
		return err
	}
	if command != "" {
		err = sm.ctx.GetRedisConn().Hset(k, FieldCommand, command)
		if err != nil {
			return err
		}
	}
	return sm.ctx.GetRedisConn().Expire(k, time.Second*stateTTL)
}

// SetField 设置状态中的某个字段
func (sm *stateMachine) SetField(uid string, spaceID string, field string, value string) error {
	k := sm.key(uid, spaceID)
	err := sm.ctx.GetRedisConn().Hset(k, field, value)
	if err != nil {
		return err
	}
	return sm.ctx.GetRedisConn().Expire(k, time.Second*stateTTL)
}

// Clear 清除用户状态
func (sm *stateMachine) Clear(uid string, spaceID string) error {
	return sm.ctx.GetRedisConn().Del(sm.key(uid, spaceID))
}

// GetCommand 获取当前正在执行的命令
func (sm *stateMachine) GetCommand(uid string, spaceID string) (string, error) {
	return sm.GetField(uid, spaceID, FieldCommand)
}

// GetBotID 获取正在操作的Bot ID
func (sm *stateMachine) GetBotID(uid string, spaceID string) (string, error) {
	return sm.GetField(uid, spaceID, FieldBotID)
}
