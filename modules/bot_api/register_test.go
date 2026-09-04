package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

func TestRegisterRejectsMalformedBodyBeforeBotLookup(t *testing.T) {
	ba := &BotAPI{}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.POST("/v1/bot/register", ba.register)

	req := httptest.NewRequest(http.MethodPost, "/v1/bot/register", strings.NewReader(`{"instance_id":`))
	req.Header.Set("Authorization", "Bearer bf_test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var env errEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "err.server.bot_api.request_invalid", env.Error.Code)
	require.Equal(t, map[string]any{"field": "body"}, env.Error.Details)
}
