package space

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	"go.uber.org/zap"
)

// 邀请码生成参数。
//
// 16 hex = 64 bit 熵（原 8 hex 仅 32 bit，见 issue #1000）。DB 列 VARCHAR(40) 兼容旧码。
const (
	inviteCodeHexLen     = 16
	inviteCodeMaxRetries = 3

	envInviteDefaultMaxUses = "DM_SPACE_INVITE_DEFAULT_MAX_USES"
	envInviteDefaultTTL     = "DM_SPACE_INVITE_DEFAULT_TTL"

	inviteDefaultTTL = 72 * time.Hour
)

// generateInviteCodeFn 包级函数变量，便于测试注入碰撞场景。
var generateInviteCodeFn = defaultGenerateInviteCode

func defaultGenerateInviteCode() (string, error) {
	buf := make([]byte, inviteCodeHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成邀请码随机字节失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// generateInviteCode 通过 generateInviteCodeFn 生成 16 hex 邀请码。
func generateInviteCode() (string, error) {
	return generateInviteCodeFn()
}

// inviteDefaults 邀请码默认 max_uses / expires_at。
// 未设置或非法环境变量时：max_uses=0（不限），expires_at=now+72h。
func inviteDefaults(now time.Time) (int, *time.Time) {
	maxUses := 0
	if v := strings.TrimSpace(os.Getenv(envInviteDefaultMaxUses)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxUses = n
		}
	}
	ttl := inviteDefaultTTL
	if v := strings.TrimSpace(os.Getenv(envInviteDefaultTTL)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ttl = d
		}
	}
	var expiresAt *time.Time
	if ttl > 0 {
		t := now.Add(ttl)
		expiresAt = &t
	}
	return maxUses, expiresAt
}

// applyInviteDefaultsIfNeeded 若调用方未显式设置，填充默认 max_uses / expires_at。
// 已设置的字段保持不变，便于管理端 API 透传用户指定的值。
func applyInviteDefaultsIfNeeded(model *InvitationModel, now time.Time) {
	defMaxUses, defExpiresAt := inviteDefaults(now)
	if model.MaxUses == 0 {
		model.MaxUses = defMaxUses
	}
	if model.ExpiresAt == nil && defExpiresAt != nil {
		t := db.Time(*defExpiresAt)
		model.ExpiresAt = &t
	}
}

// insertInvitationWithRetry 碰撞重试写入。成功返回最终 code；持续 Duplicate 则在耗尽重试后返回错误。
// 调用方可预设 SpaceId/Creator/MaxUses/ExpiresAt/Status 等字段，InviteCode 字段由本函数覆盖。
func (s *Space) insertInvitationWithRetry(model *InvitationModel) (string, error) {
	applyInviteDefaultsIfNeeded(model, time.Now())

	var lastErr error
	for attempt := 0; attempt < inviteCodeMaxRetries; attempt++ {
		code, err := generateInviteCode()
		if err != nil {
			return "", err
		}
		model.InviteCode = code
		if err := s.db.insertInvitation(model); err == nil {
			return code, nil
		} else {
			lastErr = err
			if strings.Contains(err.Error(), "Duplicate") {
				s.Warn("invite_code 碰撞，重试",
					zap.String("code", code),
					zap.Int("attempt", attempt+1),
				)
				continue
			}
			return "", err
		}
	}
	return "", fmt.Errorf("邀请码生成碰撞重试耗尽: %w", lastErr)
}
