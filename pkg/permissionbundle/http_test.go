package permissionbundle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

func TestHTTPCompletePrivateProjectionsAndEnvelope(t *testing.T) {
	s := fixtureStore(t, filepath.Join(t.TempDir(), "http.sqlite"))
	seed(t, s)
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/owner", func(c *wkhttp.Context) { s.Serve(c, "bot", "owner", "owner_get") })
	r.GET("/self", func(c *wkhttp.Context) { s.Serve(c, "bot", "", "self") })
	r.Group("").PUT("/owner", func(c *wkhttp.Context) { s.Serve(c, "bot", "owner", "mutate") })
	r.GET("/missing", func(c *wkhttp.Context) { var missing *Store; missing.Serve(c, "bot", "", "self") })
	var bundle Bundle
	for _, path := range []string{"/owner", "/self"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?space_id=space", nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &obj))
		if path == "/owner" {
			require.Len(t, obj, 3)
			require.Contains(t, obj, "contract")
			require.NoError(t, json.Unmarshal(obj["bundle"], &bundle))
		} else {
			require.Len(t, obj, 10)
			require.NotContains(t, obj, "attribution")
			var self Bundle
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &self))
			require.Equal(t, bundle, self)
		}
	}
	for name, req := range map[string]*http.Request{
		"duplicate selector": httptest.NewRequest("GET", "/self?space_id=space&space_id=space", nil),
		"conflicting selector": func() *http.Request {
			r := httptest.NewRequest("GET", "/self?space_id=space", nil)
			r.Header.Set("X-Space-ID", "other")
			return r
		}(),
		"missing authority": httptest.NewRequest("GET", "/missing?space_id=space", nil),
		"compressed": func() *http.Request {
			r := httptest.NewRequest("PUT", "/owner?space_id=space", strings.NewReader("private-marker"))
			r.Header.Set("Content-Encoding", "gzip")
			return r
		}(),
		"invalid mutation": httptest.NewRequest("PUT", "/owner?space_id=space", strings.NewReader(`{"private-marker":"secret"}`)),
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			require.Equal(t, 400, rec.Code)
			require.NotContains(t, rec.Body.String(), "private-marker")
			var obj map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &obj))
			require.Contains(t, obj, "error")
		})
	}
}

func TestNativeSQLPredicatesAndAtomicFailure(t *testing.T) {
	for name, q := range map[string]string{
		"inactive peer robot is eligible": "",
		"active peer robot":               `UPDATE robot SET status=1 WHERE robot_id='peer'`,
		"all-status app conflict":         `INSERT INTO app_bot VALUES(1,'peer',0)`,
		"destroyed peer":                  `UPDATE user SET is_destroy=2 WHERE uid='peer'`,
		"removed peer":                    `UPDATE space_member SET status=0 WHERE uid='peer'`,
	} {
		t.Run(name, func(t *testing.T) {
			s := fixtureStore(t, filepath.Join(t.TempDir(), "native.sqlite"))
			seed(t, s)
			if q != "" {
				_, e := s.Session.Exec(q)
				require.NoError(t, e)
			}
			p, e := s.Get(t.Context(), fixtureKey, "owner")
			require.NoError(t, e)
			prep, _ := json.Marshal(Preparation{fixtureKey, p.Bundle.Expected(), []string{"departed", "peer"}})
			got, e := s.Prepare(t.Context(), fixtureKey, "owner", strings.NewReader(string(prep)))
			if q == "" {
				require.NoError(t, e)
				require.Len(t, got.References, 2)
			} else {
				require.Error(t, e)
				require.Nil(t, got.References)
			}
			after, e := s.Get(t.Context(), fixtureKey, "owner")
			require.NoError(t, e)
			require.Equal(t, p, after)
		})
	}
	for _, pair := range [][2]string{{"bot", "bot"}, {"bot@alias", "peer"}, {"bot", "peer@alias"}} {
		_, e := PairReference(pair[0], pair[1])
		require.ErrorIs(t, e, ErrReference)
	}
}
