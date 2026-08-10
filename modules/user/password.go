package user

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 10

const (
	minPasswordLength = 8
	// maxPasswordLength 对齐 bcrypt 的硬上限（72 字节）。x/crypto 的
	// GenerateFromPassword 对超长输入返回 ErrPasswordTooLong；不在入参校验阶段挡住的话
	// 用户会拿到"密码处理失败"这种内部错误语义，而这本质上是一个入参问题。
	maxPasswordLength = 72
)

var (
	// ErrPasswordTooShort 密码长度不足 minPasswordLength 个字符。
	ErrPasswordTooShort = errors.New("password too short")
	// ErrPasswordTooLong 密码字节数超过 bcrypt 上限。
	ErrPasswordTooLong = errors.New("password too long")
	// ErrPasswordNotComplexEnough 密码字符类别不足两类。
	ErrPasswordNotComplexEnough = errors.New("password not complex enough")
)

// ValidatePasswordStrength 校验账号登录密码强度：长度 ≥ minPasswordLength 个字符，
// 不超过 maxPasswordLength 字节，且大写字母/小写字母/数字/特殊字符四类中至少命中 2 类。
//
// 长度下限按 rune 计（"8 位"指 8 个字符）：若按 byte 计，4 个中文字符就有 12 字节，
// 会被判定为满足"≥8"，实际强度远低于策略要求。长度上限则必须按 byte 计，因为 bcrypt
// 的 72 上限是字节数。
//
// 仅适用于账号登录密码（注册/改密/找回/管理端创建与重置）；ChatPwd/LockScreenPwd
// 等设备侧短 PIN 字段不套用此规则。
func ValidatePasswordStrength(password string) error {
	if utf8.RuneCountInString(password) < minPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > maxPasswordLength {
		return ErrPasswordTooLong
	}
	var hasUpper, hasLower, hasDigit, hasSpecial bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasSpecial = true
		}
	}
	classes := 0
	for _, ok := range []bool{hasUpper, hasLower, hasDigit, hasSpecial} {
		if ok {
			classes++
		}
	}
	if classes < 2 {
		return ErrPasswordNotComplexEnough
	}
	return nil
}

// HashPassword 使用 bcrypt 哈希密码
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword 验证密码是否匹配存储的哈希值
// 同时支持 bcrypt 和旧版 MD5(MD5(password)) 格式
// 返回: matched（是否匹配）, needsMigration（是否需要迁移到 bcrypt）
func CheckPassword(password, storedHash string) (matched bool, needsMigration bool) {
	if isBcryptHash(storedHash) {
		err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password))
		return err == nil, false
	}
	// 旧版 MD5(MD5(password)) 验证
	md5Hash := util.MD5(util.MD5(password))
	if md5Hash == storedHash {
		return true, true
	}
	return false, false
}

// isBcryptHash 判断存储的哈希值是否为 bcrypt 格式
func isBcryptHash(hash string) bool {
	return strings.HasPrefix(hash, "$2a$") ||
		strings.HasPrefix(hash, "$2b$") ||
		strings.HasPrefix(hash, "$2y$")
}
