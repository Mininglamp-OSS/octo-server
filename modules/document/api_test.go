package document

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func init() { gin.SetMode(gin.TestMode) }

func TestTenantSpaceIDUsesOnlyMiddlewareValidatedSpace(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("GET", "/v1/documents/state?space_id=forged-query", nil)
	ginCtx.Request.Header.Set("X-Space-ID", "forged-header")
	ginCtx.Set("space_id", "validated-space")
	c := &wkhttp.Context{Context: ginCtx}

	spaceID, ok := tenantSpaceID(c)

	assert.True(t, ok)
	assert.Equal(t, "validated-space", spaceID)
}

func TestTenantSpaceIDRejectsMissingMiddlewareSpace(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("GET", "/v1/documents/state?space_id=forged-query", nil)
	ginCtx.Request.Header.Set("X-Space-ID", "forged-header")
	c := &wkhttp.Context{Context: ginCtx}

	spaceID, ok := tenantSpaceID(c)

	assert.False(t, ok)
	assert.Empty(t, spaceID)
}

func TestDocumentErrorCodeClassifiesBusinessErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		mutation bool
		wantID   string
	}{
		{"space_name_duplicate", errors.New("空间名称已存在"), true, errcode.ErrDocumentRequestInvalid.ID},
		{"owner_only_denied", errors.New("仅空间所有者可操作"), true, errcode.ErrDocumentForbidden.ID},
		{"permanent_delete_guard", errors.New("只能彻底删除回收站文件"), true, errcode.ErrDocumentForbidden.ID},
		{"invalid_filename", errors.New("文件名非法"), true, errcode.ErrDocumentRequestInvalid.ID},
		{"unsupported_transfer", errors.New("暂不支持转让空间"), true, errcode.ErrDocumentRequestInvalid.ID},
		{"missing_resource", errors.New("文档不存在"), false, errcode.ErrDocumentNotFound.ID},
		{"unknown_mutation", errors.New("database timeout"), true, errcode.ErrDocumentStoreFailed.ID},
		{"unknown_query", errors.New("database timeout"), false, errcode.ErrDocumentQueryFailed.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := documentErrorCode(tt.err, tt.mutation)
			assert.Equal(t, tt.wantID, got.ID)
		})
	}
}
