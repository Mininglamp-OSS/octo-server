package doc_binding

import (
	"os"
	"strings"
	"testing"
)

// TestDocBindingNoLegacyResponseError 硬性锁定：modules/doc_binding 的 handler 文件
// 不许回落到 octo-lib 老 wkhttp 错误响应形态。所有业务错误统一走
// httperr.ResponseErrorL + errcode.ErrDocBinding*，才能被 i18n renderer 转 zh-CN
// 且不绕过 D14 双 envelope。
// 参照 modules/category/api_i18n_test.go 的实现：先去掉行注释再扫，避免注释里
// 写"以前是 c.ResponseError(...)"这种历史脚印被误判。c.Response(struct) /
// c.ResponseOK 是正常成功响应，不会匹配 "c.Response(\"" 因为它匹配的是形如
// c.Response("msg") 的字符串直传形态。
func TestDocBindingNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go"}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", "c.Response(\"", ".AbortWithStatusJSON(", ".AbortWithStatus("}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			var clean strings.Builder
			for _, line := range strings.Split(string(data), "\n") {
				if idx := strings.Index(line, "//"); idx >= 0 {
					line = line[:idx]
				}
				clean.WriteString(line)
				clean.WriteByte('\n')
			}
			cleaned := clean.String()
			for _, b := range banned {
				if strings.Contains(cleaned, b) {
					t.Fatalf("modules/doc_binding/%s must use httperr.ResponseErrorL + errcode.ErrDocBinding* instead of legacy %s", f, b)
				}
			}
		})
	}
}
