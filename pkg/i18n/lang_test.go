package i18n

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNegotiateLanguagePriority(t *testing.T) {
	cidrs, err := ParseCIDRList("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDRList err = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/?lang=en-US", nil)
	req.RemoteAddr = "10.1.2.3:4321"
	req.Header.Set(HeaderOctoLang, "zh-CN")
	req.Header.Set("Accept-Language", "en-US")
	req.AddCookie(&http.Cookie{Name: CookieLanguage, Value: "en-US"})

	got := NegotiateLanguage(req, LanguageNegotiationOptions{
		DefaultLanguage:        "en-US",
		TrustedLangHeaderCIDRs: cidrs,
		UserLanguage:           "en-US",
	})
	if got.Language != "zh-CN" || got.Source != LanguageSourceTrustedHeader {
		t.Fatalf("NegotiateLanguage = %#v, want trusted zh-CN", got)
	}
}

func TestNegotiateLanguageIgnoresUntrustedXOctoLang(t *testing.T) {
	cidrs, err := ParseCIDRList("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDRList err = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/?lang=en-US", nil)
	req.RemoteAddr = "203.0.113.9:4321"
	req.Header.Set(HeaderOctoLang, "zh-CN")
	req.Header.Set("X-Forwarded-For", "10.1.2.3")

	got := NegotiateLanguage(req, LanguageNegotiationOptions{
		DefaultLanguage:        "zh-CN",
		TrustedLangHeaderCIDRs: cidrs,
	})
	if got.Language != "en-US" || got.Source != LanguageSourceQuery {
		t.Fatalf("NegotiateLanguage = %#v, want query en-US; XFF must not make header trusted", got)
	}
}

func TestNegotiateLanguageTrustsXOctoLangThroughTrustedProxy(t *testing.T) {
	langCIDRs, err := ParseCIDRList("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDRList lang err = %v", err)
	}
	proxyCIDRs, err := ParseCIDRList("172.16.0.0/12")
	if err != nil {
		t.Fatalf("ParseCIDRList proxy err = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.16.1.10:4321"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 10.1.2.3")
	req.Header.Set(HeaderOctoLang, "zh-CN")
	req.Header.Set("Accept-Language", "en-US")

	got := NegotiateLanguage(req, LanguageNegotiationOptions{
		DefaultLanguage:        "en-US",
		TrustedLangHeaderCIDRs: langCIDRs,
		TrustedProxyCIDRs:      proxyCIDRs,
	})
	if got.Language != "zh-CN" || got.Source != LanguageSourceTrustedHeader {
		t.Fatalf("NegotiateLanguage = %#v, want trusted proxy zh-CN", got)
	}
}

func TestNegotiateLanguageIgnoresSpoofedXFFBeforeUntrustedClient(t *testing.T) {
	langCIDRs, err := ParseCIDRList("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDRList lang err = %v", err)
	}
	proxyCIDRs, err := ParseCIDRList("172.16.0.0/12")
	if err != nil {
		t.Fatalf("ParseCIDRList proxy err = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.16.1.10:4321"
	req.Header.Set("X-Forwarded-For", "10.1.2.3, 198.51.100.9")
	req.Header.Set(HeaderOctoLang, "zh-CN")
	req.Header.Set("Accept-Language", "en-US")

	got := NegotiateLanguage(req, LanguageNegotiationOptions{
		DefaultLanguage:        "zh-CN",
		TrustedLangHeaderCIDRs: langCIDRs,
		TrustedProxyCIDRs:      proxyCIDRs,
	})
	if got.Language != "en-US" || got.Source != LanguageSourceAccept {
		t.Fatalf("NegotiateLanguage = %#v, want Accept-Language en-US; spoofed XFF must not be trusted", got)
	}
}

func TestNegotiateLanguageSourceOrder(t *testing.T) {
	tests := []struct {
		name   string
		req    *http.Request
		user   string
		want   string
		source LanguageSource
	}{
		{
			name: "query beats cookie user and accept",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/?lang=zh-CN", nil)
				r.AddCookie(&http.Cookie{Name: CookieLanguage, Value: "en-US"})
				r.Header.Set("Accept-Language", "en-US")
				return r
			}(),
			user:   "en-US",
			want:   "zh-CN",
			source: LanguageSourceQuery,
		},
		{
			name: "cookie beats user and accept",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.AddCookie(&http.Cookie{Name: CookieLanguage, Value: "zh-CN"})
				r.Header.Set("Accept-Language", "en-US")
				return r
			}(),
			user:   "en-US",
			want:   "zh-CN",
			source: LanguageSourceCookie,
		},
		{
			name: "user beats accept",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Accept-Language", "zh-CN")
				return r
			}(),
			user:   "en-US",
			want:   "en-US",
			source: LanguageSourceUser,
		},
		{
			name: "accept beats default",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Accept-Language", "fr-FR, zh-CN;q=0.8, en-US;q=0.5")
				return r
			}(),
			want:   "zh-CN",
			source: LanguageSourceAccept,
		},
		{
			name:   "default",
			req:    httptest.NewRequest(http.MethodGet, "/", nil),
			want:   "zh-CN",
			source: LanguageSourceDefault,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NegotiateLanguage(tt.req, LanguageNegotiationOptions{
				DefaultLanguage: "zh-CN",
				UserLanguage:    tt.user,
			})
			if got.Language != tt.want || got.Source != tt.source {
				t.Fatalf("NegotiateLanguage = %#v, want %s from %s", got, tt.want, tt.source)
			}
		})
	}
}

func TestNegotiateLanguageUsesRuntimeDefaultWhenNoConfiguredDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	got := NegotiateLanguage(req, LanguageNegotiationOptions{})
	if got.Language != DefaultLanguage || got.Source != LanguageSourceDefault {
		t.Fatalf("NegotiateLanguage = %#v, want %s from %s", got, DefaultLanguage, LanguageSourceDefault)
	}
}

func TestMatchSupportedLanguage(t *testing.T) {
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"zh-CN", "zh-CN", true},
		{"zh_cn", "zh-CN", true},
		{"zh", "zh-CN", true},
		{"en", "en-US", true},
		{"fr-FR", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := MatchSupportedLanguage(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("MatchSupportedLanguage(%q) = %q, %v; want %q, %v",
					tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseCIDRList(t *testing.T) {
	cidrs, err := ParseCIDRList("10.0.0.0/8, 192.168.1.0/24")
	if err != nil {
		t.Fatalf("ParseCIDRList err = %v", err)
	}
	if len(cidrs) != 2 {
		t.Fatalf("ParseCIDRList len = %d, want 2", len(cidrs))
	}
	if _, err := ParseCIDRList("10.0.0.1"); err == nil {
		t.Fatal("ParseCIDRList accepted bare IP; want CIDR error")
	}
}
