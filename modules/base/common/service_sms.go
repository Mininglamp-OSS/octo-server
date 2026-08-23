package common

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

type ISMSProvider interface {
	SendSMS(ctx context.Context, zone, phone string, code string) error
}

// ISMSService ISMSService
type ISMSService interface {
	// 发送验证码
	SendVerifyCode(ctx context.Context, zone, phone string, codeType CodeType) error
	// 验证验证码(销毁缓存)
	Verify(ctx context.Context, zone, phone, code string, codeType CodeType) error
}

// SMSService 短信服务
type SMSService struct {
	ctx *config.Context
	log.Log
}

// NewSMSService 创建短信服务
func NewSMSService(ctx *config.Context) *SMSService {
	return &SMSService{
		ctx: ctx,
		Log: log.NewTLog("SMSService"),
	}
}

// SendVerifyCode 发送验证码
func (s *SMSService) SendVerifyCode(ctx context.Context, zone, phone string, codeType CodeType) error {
	var smsProvider ISMSProvider
	// 检查发送频率限制
	rateLimitKey := fmt.Sprintf("sms_rate_limit:%s@%s", zone, phone)
	exists, err := s.ctx.GetRedisConn().GetString(rateLimitKey)
	if err != nil {
		return err
	}
	if exists != "" {
		return errors.New("发送过于频繁，请1分钟后再试")
	}

	smsProviderName := s.ctx.GetConfig().SMSProvider
	switch smsProviderName {
	case config.SMSProviderAliyun:
		if zone != "0086" && s.ctx.GetConfig().AliyunInternationalSMS.AccessKeyID != "" {
			smsProvider = NewAliyunInternationalProvider(s.ctx)
		} else {
			smsProvider = NewAliyunProvider(s.ctx)
		}
	case config.SMSProviderUnisms:
		smsProvider = NewUnismsProvider(s.ctx)
	case config.SMSProviderSmsbao:
		smsProvider = NewSmsbaoProvider(s.ctx)
	}

	if smsProvider == nil {
		return errors.New("没有找到短信提供商！")
	}

	verifyCode := ""
	// rand.Seed(int64(time.Now().Nanosecond()))
	// for i := 0; i < 4; i++ {
	// 	verifyCode += fmt.Sprintf("%v", rand.Intn(10))
	// }
	// 使用 crypto/rand 生成安全的验证码
	verifyCode, err = generateSecureVerifyCode(4)
	if err != nil {
		s.Error("生成验证码失败", zap.Error(err))
		return errors.New("系统错误，请稍后重试")
	}
	s.Info("发送验证码", zap.String("phone", phone))
	cacheKey := fmt.Sprintf("%s%d@%s@%s", CacheKeySMSCode, codeType, zone, phone)
	err = s.ctx.GetRedisConn().SetAndExpire(cacheKey, verifyCode, time.Minute*5)
	if err != nil {
		return err
	}

	// 设置发送频率限制
	err = s.ctx.GetRedisConn().SetAndExpire(rateLimitKey, "1", time.Minute)
	if err != nil {
		return err
	}

	err = smsProvider.SendSMS(ctx, zone, phone, verifyCode)
	return err
}

// GenerateVerifyCode exposes the shared cryptographically-secure numeric code
// generator to callers outside this package.
//
// It exists so a flow that needs its own verification-code keyspace (the
// manager-console second factor, whose buckets must not be reachable from the
// public email endpoints) can still use the same generator rather than growing a
// second implementation with its own chance of getting randomness wrong.
func GenerateVerifyCode(length int) (string, error) {
	return generateSecureVerifyCode(length)
}

// generateSecureVerifyCode 生成密码学安全的验证码
func generateSecureVerifyCode(length int) (string, error) {
	const digits = "0123456789"
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			return "", err
		}
		result[i] = digits[num.Int64()]
	}
	return string(result), nil
}

// Verify 验证验证码
func (s *SMSService) Verify(ctx context.Context, zone, phone, code string, codeType CodeType) error {
	span, _ := s.ctx.Tracer().StartSpanFromContext(ctx, "smsService.Verify")
	defer span.Finish()

	// 检查是否被锁定
	lockKey := fmt.Sprintf("sms_verify_lock:%s@%s", zone, phone)
	locked, err := s.ctx.GetRedisConn().GetString(lockKey)
	if err != nil {
		return err
	}
	if locked != "" {
		return errors.New("验证失败次数过多，请10分钟后再试")
	}

	cacheKey := fmt.Sprintf("%s%d@%s@%s", CacheKeySMSCode, codeType, zone, phone)
	sysCode, err := s.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil {
		return err
	}
	// Delete the code immediately after reading to minimize the TOCTOU window.
	// This ensures a concurrent request will see an empty key.
	// NOTE: For a fully atomic solution, dmwork-lib should expose GetDel or Eval.
	if sysCode != "" {
		s.ctx.GetRedisConn().Del(cacheKey)
	}
	if sysCode != "" && subtle.ConstantTimeCompare([]byte(sysCode), []byte(code)) == 1 {
		// 验证成功，清除失败计数
		failCountKey := fmt.Sprintf("sms_verify_fail:%s@%s", zone, phone)
		s.ctx.GetRedisConn().Del(failCountKey)
		s.ctx.GetRedisConn().Del(lockKey)
		return nil
	}

	// 验证失败，增加失败计数
	failCountKey := fmt.Sprintf("sms_verify_fail:%s@%s", zone, phone)
	failCountStr, _ := s.ctx.GetRedisConn().GetString(failCountKey)
	failCount := 0
	if failCountStr != "" {
		if count, err := strconv.Atoi(failCountStr); err == nil {
			failCount = count
		}
	}
	failCount++

	if failCount >= 3 {
		// 锁定10分钟
		s.ctx.GetRedisConn().SetAndExpire(lockKey, "1", time.Minute*10)
		//s.ctx.GetRedisConn().Del(failCountKey)
		return errors.New("验证失败次数过多，已锁定10分钟")
	} else {
		// 设置失败计数，10分钟过期
		s.ctx.GetRedisConn().SetAndExpire(failCountKey, fmt.Sprintf("%d", failCount), time.Minute*10)
	}

	maskedPhone := phone
	if len(phone) >= 7 {
		maskedPhone = phone[:3] + "****" + phone[len(phone)-4:]
	} else if len(phone) > 2 {
		maskedPhone = phone[:1] + "****"
	}
	s.Info("验证码错误", zap.String("phone", maskedPhone))
	return errors.New("验证码无效！")
}
