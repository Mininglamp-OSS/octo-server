package errcode

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestUserCodesHaveZhCNTranslations(t *testing.T) {
	localizer := i18n.NewLocalizer(i18n.DefaultLanguage)
	for _, code := range []struct {
		id     string
		source string
		zhCN   string
	}{
		{ErrUserRequestInvalid.ID, ErrUserRequestInvalid.DefaultMessage, "请求数据格式有误。"},
		{ErrUserLockMinuteOutOfRange.ID, ErrUserLockMinuteOutOfRange.DefaultMessage, "锁屏时长必须在 0 到 60 分钟之间。"},
		{ErrUserShortNoFormatInvalid.ID, ErrUserShortNoFormatInvalid.DefaultMessage, "短号必须以字母开头，长度 6-20 位，仅支持字母、数字、下划线或减号。"},
		{ErrUserLanguageUnsupported.ID, ErrUserLanguageUnsupported.DefaultMessage, "不支持的语言。"},
		{ErrUserTokenRequired.ID, ErrUserTokenRequired.DefaultMessage, "Token 不能为空。"},
		{ErrUserInvalidCredentials.ID, ErrUserInvalidCredentials.DefaultMessage, "用户名或密码错误。"},
		{ErrUserCodeInvalid.ID, ErrUserCodeInvalid.DefaultMessage, "验证码错误。"},
		{ErrUserAccountBanned.ID, ErrUserAccountBanned.DefaultMessage, "此账号已被封禁。"},
		{ErrUserLoginDeviceExpired.ID, ErrUserLoginDeviceExpired.DefaultMessage, "登录设备已过期，请重新登录。"},
		{ErrUserLoginLocked.ID, ErrUserLoginLocked.DefaultMessage, "登录失败次数过多，账号已被临时锁定，请稍后再试。"},
		{ErrUserNotFound.ID, ErrUserNotFound.DefaultMessage, "用户不存在。"},
		{ErrUserCurrentNotFound.ID, ErrUserCurrentNotFound.DefaultMessage, "当前登录用户不存在。"},
		{ErrUserAlreadyExists.ID, ErrUserAlreadyExists.DefaultMessage, "该用户已存在。"},
		{ErrUserRegistrationClosed.ID, ErrUserRegistrationClosed.DefaultMessage, "注册通道暂未开放。"},
		{ErrUserLocalLoginDisabled.ID, ErrUserLocalLoginDisabled.DefaultMessage, "本地登录已关闭。"},
		{ErrUserPhoneRegionUnsupported.ID, ErrUserPhoneRegionUnsupported.DefaultMessage, "仅支持中国大陆手机号。"},
		{ErrUserInviteCodeNotFound.ID, ErrUserInviteCodeNotFound.DefaultMessage, "邀请码不存在。"},
		{ErrUserAccountDestroyed.ID, ErrUserAccountDestroyed.DefaultMessage, "该账号已注销。"},
		{ErrUserAccountDestroying.ID, ErrUserAccountDestroying.DefaultMessage, "账号正处于注销冷静期，请使用新版客户端撤销或查询状态。"},
		{ErrUserUpdateNotAllowed.ID, ErrUserUpdateNotAllowed.DefaultMessage, "该字段不允许修改。"},
		{ErrUserShortNoAlreadyChanged.ID, ErrUserShortNoAlreadyChanged.DefaultMessage, "短号仅可修改一次。"},
		{ErrUserDemoLockUnsupported.ID, ErrUserDemoLockUnsupported.DefaultMessage, "演示账号不支持开启设备锁。"},
		{ErrUserAuthCodeNotFound.ID, ErrUserAuthCodeNotFound.DefaultMessage, "授权码无效或已过期。"},
		{ErrUserAuthCodeWrongType.ID, ErrUserAuthCodeWrongType.DefaultMessage, "授权码类型不是登录授权码。"},
		{ErrUserAuthInfoInvalid.ID, ErrUserAuthInfoInvalid.DefaultMessage, "授权信息格式错误。"},
		{ErrUserAuthScannerMismatch.ID, ErrUserAuthScannerMismatch.DefaultMessage, "扫描者与授权者不是同一用户。"},
		{ErrUserQRVerCodeMissing.ID, ErrUserQRVerCodeMissing.DefaultMessage, "用户没有 QR 验证码。"},
		{ErrUserWeChatExchangeFailed.ID, ErrUserWeChatExchangeFailed.DefaultMessage, "获取微信 access_token 失败。"},
		{ErrUserWeChatProfileFailed.ID, ErrUserWeChatProfileFailed.DefaultMessage, "获取微信用户资料失败。"},
		{ErrUserWeChatResponseInvalid.ID, ErrUserWeChatResponseInvalid.DefaultMessage, "微信返回数据格式错误。"},
		{ErrUserChatPwdUpdateFailed.ID, ErrUserChatPwdUpdateFailed.DefaultMessage, "修改聊天密码失败。"},
		{ErrUserLoginPwdUpdateFailed.ID, ErrUserLoginPwdUpdateFailed.DefaultMessage, "修改登录密码失败。"},
		{ErrUserLockScreenPwdUpdateFailed.ID, ErrUserLockScreenPwdUpdateFailed.DefaultMessage, "修改锁屏密码失败。"},
		{ErrUserPasswordProcessFailed.ID, ErrUserPasswordProcessFailed.DefaultMessage, "密码处理失败。"},
		{ErrUserQueryFailed.ID, ErrUserQueryFailed.DefaultMessage, "查询用户数据失败。"},
		{ErrUserStoreFailed.ID, ErrUserStoreFailed.DefaultMessage, "存储用户数据失败。"},
		{ErrUserIMCallFailed.ID, ErrUserIMCallFailed.DefaultMessage, "调用 IM 服务失败。"},
		{ErrUserDecodeFailed.ID, ErrUserDecodeFailed.DefaultMessage, "解码内部数据失败。"},
		{ErrUserFileOperationFailed.ID, ErrUserFileOperationFailed.DefaultMessage, "文件处理失败。"},
		{ErrUserSMSSendFailed.ID, ErrUserSMSSendFailed.DefaultMessage, "短信发送失败。"},
		{ErrUserDestroyFailed.ID, ErrUserDestroyFailed.DefaultMessage, "注销账号失败。"},
		{ErrUserRegisterFailed.ID, ErrUserRegisterFailed.DefaultMessage, "注册失败。"},
		{ErrUserLanguageSetFailed.ID, ErrUserLanguageSetFailed.DefaultMessage, "设置语言偏好失败。"},
	} {
		t.Run(code.id, func(t *testing.T) {
			got := localizer.Translate(code.id, "zh-CN", nil)
			if got != code.zhCN {
				t.Fatalf("zh-CN translation = %q, want %q", got, code.zhCN)
			}
			if got == code.id || strings.EqualFold(got, code.source) {
				t.Fatalf("zh-CN translation for %s fell back to %q", code.id, got)
			}
		})
	}
}
