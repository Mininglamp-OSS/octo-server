package errcode

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestThreadCodesHaveZhCNTranslations(t *testing.T) {
	localizer := i18n.NewLocalizer(i18n.DefaultLanguage)
	for _, code := range []struct {
		id     string
		source string
		zhCN   string
	}{
		{ErrThreadGroupNoInvalid.ID, ErrThreadGroupNoInvalid.DefaultMessage, "群编号无效。"},
		{ErrThreadShortIDInvalid.ID, ErrThreadShortIDInvalid.DefaultMessage, "子区短 ID 无效。"},
		{ErrThreadRequestInvalid.ID, ErrThreadRequestInvalid.DefaultMessage, "请求无效。"},
		{ErrThreadNameInvalid.ID, ErrThreadNameInvalid.DefaultMessage, "子区名称不能为空，且不能超过 100 个字符。"},
		{ErrThreadSourceMessageInvalid.ID, ErrThreadSourceMessageInvalid.DefaultMessage, "来源消息内容无效。"},
		{ErrThreadStatusInvalid.ID, ErrThreadStatusInvalid.DefaultMessage, "子区状态无效。"},
		{ErrThreadNotGroupMember.ID, ErrThreadNotGroupMember.DefaultMessage, "你不是群成员。"},
		{ErrThreadPermissionDenied.ID, ErrThreadPermissionDenied.DefaultMessage, "无权执行此子区操作。"},
		{ErrThreadNotFound.ID, ErrThreadNotFound.DefaultMessage, "子区不存在。"},
		{ErrThreadDeleted.ID, ErrThreadDeleted.DefaultMessage, "子区已被删除。"},
		{ErrThreadNotActive.ID, ErrThreadNotActive.DefaultMessage, "子区不是活跃状态。"},
		{ErrThreadStatusChanged.ID, ErrThreadStatusChanged.DefaultMessage, "子区状态已变化，请重试。"},
		{ErrThreadCreatorCannotLeave.ID, ErrThreadCreatorCannotLeave.DefaultMessage, "子区创建者不能退出子区。"},
		{ErrThreadGroupMDNotFound.ID, ErrThreadGroupMDNotFound.DefaultMessage, "子区 GROUP.md 不存在。"},
		{ErrThreadGroupMDContentEmpty.ID, ErrThreadGroupMDContentEmpty.DefaultMessage, "GROUP.md 内容不能为空。"},
		{ErrThreadGroupMDContentTooLarge.ID, ErrThreadGroupMDContentTooLarge.DefaultMessage, "GROUP.md 内容超过大小限制。"},
		{ErrThreadSettingInvalid.ID, ErrThreadSettingInvalid.DefaultMessage, "子区设置无效。"},
		{ErrThreadStoreFailed.ID, ErrThreadStoreFailed.DefaultMessage, "子区存储操作失败。"},
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
