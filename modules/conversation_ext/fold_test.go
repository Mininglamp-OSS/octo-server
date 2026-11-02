package conversation_ext

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

type foldMemory struct{ rows map[string]FoldedChat }

func (m *foldMemory) ListFolded(uid, space string) ([]FoldedChat, error) {
	rows := []FoldedChat{}
	for key, row := range m.rows {
		if key == uid+space+row.ChannelID {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (m *foldMemory) SetFolded(uid, space string, chat FoldedChat, folded bool) error {
	key := uid + space + chat.ChannelID
	if folded {
		m.rows[key] = chat
	} else {
		delete(m.rows, key)
	}
	return nil
}
func TestFoldHandlers(t *testing.T) {
	store := &foldMemory{rows: map[string]FoldedChat{}}
	api := &foldedAPI{db: store, authorize: func(uid, space string, c FoldedChat) error {
		if c.ChannelID == "denied" {
			return ErrChannelForbidden
		}
		return nil
	}}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	g := r.Group("/fold", func(c *wkhttp.Context) { c.Set("uid", "u"); c.Set("space_id", c.GetHeader("X-Space-Id")); c.Next() })
	g.PUT("", api.set)
	g.GET("", api.list)
	request := func(method, space, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/fold", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Space-Id", space)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{`{}`, `{"channel_id":"g","channel_type":1,"folded":true}`, `{"channel_id":"denied","channel_type":2,"folded":true}`, `{"channel_id":"g","channel_type":2}`} {
		require.Equal(t, 400, request("PUT", "s", body).Code)
	}
	require.Empty(t, store.rows)
	require.Equal(t, 400, request("PUT", "", `{"channel_id":"g","channel_type":2,"folded":true}`).Code)
	for i := 0; i < 2; i++ {
		require.Equal(t, 200, request("PUT", "s", `{"channel_id":"g","channel_type":2,"folded":true}`).Code)
	}
	require.Len(t, store.rows, 1)
	require.Equal(t, "[]", request("GET", "other", "").Body.String())
	require.Contains(t, request("GET", "s", "").Body.String(), `"channel_id":"g"`)
	require.Equal(t, 200, request("PUT", "other", `{"channel_id":"g","channel_type":2,"folded":false}`).Code)
	require.Len(t, store.rows, 1)
	require.Equal(t, 200, request("PUT", "s", `{"channel_id":"g","channel_type":2,"folded":false}`).Code)
	require.Empty(t, store.rows)
}
func TestFoldAuthorizationFailsClosed(t *testing.T) {
	s := &Service{}
	require.Error(t, s.authorizeFold("u", "s", FoldedChat{ChannelID: "g", ChannelType: 2}))
	require.Error(t, s.authorizeFold("u", "s", FoldedChat{ChannelID: "malformed", ChannelType: 5}))
}

func TestFoldAuthorizationErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name            string
		channelType     int
		channelID       string
		authErr         error
		code            string
		status          int
		authorizeCalled bool
	}{
		{"group forbidden", 2, "g", fmt.Errorf("membership: %w", ErrChannelForbidden), "err.shared.auth.forbidden", 403, true},
		{"thread forbidden", 5, "g____1", fmt.Errorf("thread: %w", ErrThreadForbidden), "err.shared.auth.forbidden", 403, true},
		{"group database failure", 2, "g", errors.New("sensitive database timeout"), "err.server.message.query_failed", 500, true},
		{"thread database failure", 5, "g____1", errors.New("sensitive database timeout"), "err.server.message.query_failed", 500, true},
		{"missing checker", 2, "g", ErrDefaultFollowedGuardNotConfigured, "err.server.message.query_failed", 500, true},
		{"invalid thread", 5, "malformed", nil, "err.server.message.request_invalid", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &foldMemory{rows: map[string]FoldedChat{}}
			called := false
			api := &foldedAPI{db: store, authorize: func(_, _ string, _ FoldedChat) error { called = true; return tc.authErr }}
			r := wkhttp.New()
			r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
			g := r.Group("/fold", func(c *wkhttp.Context) { c.Set("uid", "u"); c.Set("space_id", "s"); c.Next() })
			g.PUT("", api.set)
			body := fmt.Sprintf(`{"channel_id":%q,"channel_type":%d,"folded":true}`, tc.channelID, tc.channelType)
			req := httptest.NewRequest("PUT", "/fold", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, 400, w.Code) // D14 wire compatibility; inspect the semantic status.
			var envelope struct {
				Error struct {
					Code       string `json:"code"`
					HTTPStatus int    `json:"http_status"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
			require.Equal(t, tc.code, envelope.Error.Code)
			require.Equal(t, tc.status, envelope.Error.HTTPStatus)
			require.Equal(t, tc.authorizeCalled, called)
			require.NotContains(t, w.Body.String(), "sensitive database timeout")
			require.Empty(t, store.rows)
		})
	}
}
