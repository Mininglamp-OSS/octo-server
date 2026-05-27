package i18n

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

func TestEarlyMiddlewareNegotiatesLanguageIntoRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(EarlyMiddleware(MiddlewareOptions{DefaultLanguage: SourceLanguage}))
	r.GET("/x", func(c *gin.Context) {
		decision, ok := LanguageFromContext(c.Request.Context())
		if !ok {
			t.Fatal("language decision missing")
		}
		if decision.Language != "zh-CN" {
			t.Fatalf("language = %q, want zh-CN", decision.Language)
		}
		if decision.Source != LanguageSourceAccept {
			t.Fatalf("source = %q, want %q", decision.Source, LanguageSourceAccept)
		}
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Language"); got != "zh-CN" {
		t.Fatalf("Content-Language = %q, want zh-CN", got)
	}
}

// TestEarlyMiddlewareContentLanguageReflectsUserMerge covers the deferred
// Content-Language refresh: after AuthMiddleware-like code injects a
// UserInfo whose Language outranks the pre-auth decision, the response
// header must advertise the merged language regardless of how the handler
// writes the body. This guards against the regression flagged in PR #181
// review where success responses kept the pre-auth Content-Language even
// when LanguageFromContext promoted to LanguageSourceUser.
func TestEarlyMiddlewareContentLanguageReflectsUserMerge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// simulateAuth stands in for octo-lib AuthMiddleware: it replaces
	// c.Request with one carrying UserInfo on the context. The writer
	// wrapper must read the *latest* request context, not a snapshot
	// captured at wrap time.
	simulateAuth := func(lang string) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Request = c.Request.WithContext(
				wkhttp.WithUser(c.Request.Context(), wkhttp.UserInfo{UID: "u1", Language: lang}),
			)
			c.Next()
		}
	}

	cases := []struct {
		name       string
		earlyLang  string // negotiated language hint via Accept-Language
		userLang   string
		write      func(*gin.Context)
		wantHeader string
	}{
		{
			name:      "Write_json_promotes",
			earlyLang: "en-US",
			userLang:  "zh-CN",
			write: func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			},
			wantHeader: "zh-CN",
		},
		{
			name:      "WriteString_promotes",
			earlyLang: "en-US",
			userLang:  "zh-CN",
			write: func(c *gin.Context) {
				c.String(http.StatusOK, "hello")
			},
			wantHeader: "zh-CN",
		},
		{
			name:      "WriteHeaderNow_noBody_promotes",
			earlyLang: "en-US",
			userLang:  "zh-CN",
			write: func(c *gin.Context) {
				c.Status(http.StatusNoContent)
				c.Writer.WriteHeaderNow()
			},
			wantHeader: "zh-CN",
		},
		{
			name:      "user_lang_matches_early_no_op",
			earlyLang: "en-US",
			userLang:  "en-US",
			write: func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			},
			wantHeader: "en-US",
		},
		{
			name:      "empty_user_lang_keeps_early",
			earlyLang: "en-US",
			userLang:  "",
			write: func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			},
			wantHeader: "en-US",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(EarlyMiddleware(MiddlewareOptions{DefaultLanguage: SourceLanguage}))
			r.Use(simulateAuth(tc.userLang))
			r.GET("/x", tc.write)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Accept-Language", tc.earlyLang)
			r.ServeHTTP(rec, req)

			if got := rec.Header().Get("Content-Language"); got != tc.wantHeader {
				t.Fatalf("Content-Language = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

// TestEarlyMiddlewareContentLanguageDoesNotDowngrade ensures explicit
// cookie / trusted-header / query selections continue to win over the
// user preference at the merge stage — the read-side merge must respect
// the same priority ladder applied at NegotiateLanguage time.
func TestEarlyMiddlewareContentLanguageDoesNotDowngrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(EarlyMiddleware(MiddlewareOptions{DefaultLanguage: SourceLanguage}))
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			wkhttp.WithUser(c.Request.Context(), wkhttp.UserInfo{UID: "u1", Language: "en-US"}),
		)
		c.Next()
	})
	r.GET("/x", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: CookieLanguage, Value: "zh-CN"})
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Language"); got != "zh-CN" {
		t.Fatalf("Content-Language = %q, want zh-CN (cookie must outrank user)", got)
	}
}

func TestEarlyMiddlewareUsesDefaultWhenNoSignal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(EarlyMiddleware(MiddlewareOptions{DefaultLanguage: "zh-CN"}))
	r.GET("/x", func(c *gin.Context) {
		decision, ok := LanguageFromContext(c.Request.Context())
		if !ok {
			t.Fatal("language decision missing")
		}
		if decision.Language != "zh-CN" {
			t.Fatalf("language = %q, want zh-CN", decision.Language)
		}
		if decision.Source != LanguageSourceDefault {
			t.Fatalf("source = %q, want %q", decision.Source, LanguageSourceDefault)
		}
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(rec, req)
}
