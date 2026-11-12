package file

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex.mlamp.cn/dmwork/octo-auth-sdk/contract"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/stretchr/testify/require"
)

const (
	fileCLIV2ServiceSecret = "cliv2-file-test-service-secret-at-least-32-bytes"
	fileCLIV2BusinessToken = "cliv2-file-business-token"
)

func fileCLIV2Router(t *testing.T) *wkhttp.WKHttp {
	t.Helper()
	source := wkhttp.New()
	cliv2.Add(source, cliv2.Route{
		Method:   http.MethodPost,
		Path:     "/v2/cli/files",
		Action:   "im.files:write",
		User:     CLIV2UploadHandler(nil, nil),
		UserOnly: true,
		Middlewares: []wkhttp.HandlerFunc{
			rejectCLIV2CallerObjectKey(),
		},
	})
	cliv2.Add(source, cliv2.Route{
		Method:   http.MethodGet,
		Path:     "/v2/cli/uploads/credentials",
		Action:   "im.files:write",
		User:     CLIV2CredentialsHandler(nil),
		UserOnly: true,
		Middlewares: []wkhttp.HandlerFunc{
			rejectCLIV2CallerObjectKey(),
		},
	})
	cliv2.Add(source, cliv2.Route{
		Method:   http.MethodGet,
		Path:     "/v2/cli/uploads/_presigned",
		Action:   "im.files:write",
		User:     CLIV2PresignedUploadHandler(nil, nil),
		UserOnly: true,
		Middlewares: []wkhttp.HandlerFunc{
			rejectCLIV2CallerObjectKey(),
		},
	})

	access := contract.AccessContext{
		Principal: contract.Principal{
			TokenID: "file-cli-token", SpaceID: "sp_file", OwnerID: "u_file_owner",
			OwnerName: "File Owner", SubjectType: "user", SubjectID: "u_file_owner",
		},
		Permissions: []contract.PermissionGroup{{Domain: "im.files", Actions: []string{"im.files:write"}}},
		BotBindings: make([]contract.BotBinding, 0),
	}
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.Data[contract.AccessContext]{Data: access})
	}))
	t.Cleanup(authServer.Close)

	target, err := cliv2.NewRouter(authServer.URL, fileCLIV2ServiceSecret)
	require.NoError(t, err)
	require.NoError(t, cliv2.SetUIDRateLimiter(target, func(c *wkhttp.Context) {
		if c.GetLoginUID() == "" {
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		c.Next()
	}))
	require.NoError(t, cliv2.Mount(source, target))
	return target
}

func fileCLIV2Request(t *testing.T, h http.Handler, method, path, contentType string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body.Bytes())
	} else {
		reader = bytes.NewReader(nil)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+fileCLIV2BusinessToken)
	h.ServeHTTP(recorder, req)
	return recorder
}

func requireCLIV2ValidationError(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	require.Equal(t, "VALIDATION_ERROR", got.Error.Code)
	return got.Error.Message
}

func TestCLIV2CredentialsPolicyIsScopedToOnePutObject(t *testing.T) {
	options := cliv2COSCredentialOptions("ap-guangzhou", "1250000000", "octo-1250000000", "chat/ts/random/file.pdf")
	require.EqualValues(t, 1800, options.DurationSeconds)
	require.Len(t, options.Policy.Statement, 1)
	require.Equal(t, []string{"cos:PutObject"}, options.Policy.Statement[0].Action)
	require.Equal(t, "allow", options.Policy.Statement[0].Effect)
	require.Equal(t, []string{"qcs::cos:ap-guangzhou:uid/1250000000:octo-1250000000/chat/ts/random/file.pdf"}, options.Policy.Statement[0].Resource)
}

func TestCLIV2FileUploadHandlersValidateInputs(t *testing.T) {
	h := fileCLIV2Router(t)

	t.Run("multipart upload requires a file", func(t *testing.T) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		require.NoError(t, form.Close())
		recorder := fileCLIV2Request(t, h, http.MethodPost, "/v2/cli/files", form.FormDataContentType(), &body)
		requireCLIV2ValidationError(t, recorder)
	})

	t.Run("STS upload credentials require a filename", func(t *testing.T) {
		recorder := fileCLIV2Request(t, h, http.MethodGet, "/v2/cli/uploads/credentials", "", nil)
		requireCLIV2ValidationError(t, recorder)
	})

	t.Run("STS credentials preserve the legacy input contract", func(t *testing.T) {
		recorder := fileCLIV2Request(t, h, http.MethodGet, "/v2/cli/uploads/credentials?filename=report.pdf", "", nil)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
		var got struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
		require.Equal(t, "UPSTREAM_UNAVAILABLE", got.Error.Code)
	})

	t.Run("presigned upload requires a byte limit", func(t *testing.T) {
		recorder := fileCLIV2Request(t, h, http.MethodGet, "/v2/cli/uploads/_presigned?filename=report.pdf", "", nil)
		requireCLIV2ValidationError(t, recorder)
	})

	t.Run("multipart upload cannot overwrite a caller-selected object key", func(t *testing.T) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		require.NoError(t, form.Close())
		message := requireCLIV2ValidationError(t, fileCLIV2Request(
			t, h, http.MethodPost,
			"/v2/cli/files?path=chat/sp_other/known-object",
			form.FormDataContentType(), &body,
		))
		require.Equal(t, "Caller-provided object keys are not supported.", message)
	})

	t.Run("STS credentials cannot sign a caller-selected object key", func(t *testing.T) {
		message := requireCLIV2ValidationError(t, fileCLIV2Request(
			t, h, http.MethodGet,
			"/v2/cli/uploads/credentials?filename=report.pdf&path=chat/sp_other/known-object",
			"", nil,
		))
		require.Equal(t, "Caller-provided object keys are not supported.", message)
	})

	t.Run("presigned upload cannot sign a caller-selected object key", func(t *testing.T) {
		message := requireCLIV2ValidationError(t, fileCLIV2Request(
			t, h, http.MethodGet,
			"/v2/cli/uploads/_presigned?filename=report.pdf&file_size=1&path=chat/sp_other/known-object",
			"", nil,
		))
		require.Equal(t, "Caller-provided object keys are not supported.", message)
	})
}
