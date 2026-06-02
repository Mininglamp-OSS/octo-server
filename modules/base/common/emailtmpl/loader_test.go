package emailtmpl

import (
	htmltemplate "html/template"
	"strings"
	"testing"
	"testing/fstest"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestRenderVerifyCode(t *testing.T) {
	tests := []struct {
		name        string
		lang        string
		wantSubject string
		wantInHTML  string
		wantInText  string
	}{
		{
			name:        "zh-CN",
			lang:        "zh-CN",
			wantSubject: "Octo 验证码",
			wantInHTML:  "您的验证码为",
			wantInText:  "您的 Octo 验证码为：123456",
		},
		{
			name:        "en-US",
			lang:        "en-US",
			wantSubject: "Octo verification code",
			wantInHTML:  "Your verification code is",
			wantInText:  "Your Octo verification code is: 123456",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(KeyVerifyCode, tt.lang, VerifyCodeData{Code: "123456"})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got.Subject != tt.wantSubject {
				t.Errorf("Subject = %q, want %q", got.Subject, tt.wantSubject)
			}
			if !strings.Contains(got.HTML, "123456") || !strings.Contains(got.HTML, tt.wantInHTML) {
				t.Errorf("HTML missing code or marker: %q", got.HTML)
			}
			if !strings.Contains(got.Text, tt.wantInText) {
				t.Errorf("Text = %q, want substring %q", got.Text, tt.wantInText)
			}
		})
	}
}

func TestRenderSpaceInviteOwner(t *testing.T) {
	data := SpaceInviteOwnerData{
		InviterName: "Alice",
		PlannedName: "Team A",
		PlannedDesc: "desc",
		AcceptURL:   htmltemplate.URL("https://x.test/v1/space/email-invite?token=abc&lang=en-US"),
	}
	got, err := Render(KeySpaceInviteOwner, "en-US", data)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got.Subject, "Team A") {
		t.Errorf("Subject = %q, want Team A", got.Subject)
	}
	// In HTML, "&" is correctly entity-escaped to "&amp;" (browsers decode it
	// back); template.URL only suppresses URL *filtering*, not HTML escaping.
	for _, want := range []string{"Alice", "Team A", "desc", "token=abc&amp;lang=en-US"} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("HTML missing %q: %q", want, got.HTML)
		}
	}
	// Plaintext keeps the raw ampersand.
	if !strings.Contains(got.Text, "https://x.test/v1/space/email-invite?token=abc&lang=en-US") {
		t.Errorf("Text missing accept URL: %q", got.Text)
	}
}

// Empty InviterName must fall back to a *localized* admin label, not a blank
// or a hardcoded Chinese string in the en-US path.
func TestRenderOwnerInviterFallbackLocalized(t *testing.T) {
	data := SpaceInviteOwnerData{PlannedName: "S"}
	en, err := Render(KeySpaceInviteOwner, "en-US", data)
	if err != nil {
		t.Fatalf("Render en: %v", err)
	}
	if !strings.Contains(en.HTML, "An Octo admin") {
		t.Errorf("en fallback inviter missing: %q", en.HTML)
	}
	zh, err := Render(KeySpaceInviteOwner, "zh-CN", data)
	if err != nil {
		t.Fatalf("Render zh: %v", err)
	}
	if !strings.Contains(zh.HTML, "Octo 管理员") {
		t.Errorf("zh fallback inviter missing: %q", zh.HTML)
	}
}

func TestRenderMemberRoleLabelLocalized(t *testing.T) {
	tests := []struct {
		lang    string
		admin   bool
		wantSub string // role label substring expected in HTML
	}{
		{"zh-CN", true, "管理员"},
		{"zh-CN", false, "成员"},
		{"en-US", true, "administrator"},
		{"en-US", false, "member"},
	}
	for _, tt := range tests {
		got, err := Render(KeySpaceInviteMember, tt.lang, SpaceInviteMemberData{
			SpaceName: "S",
			IsAdmin:   tt.admin,
			AcceptURL: htmltemplate.URL("https://x.test/?token=t"),
		})
		if err != nil {
			t.Fatalf("Render %s admin=%v: %v", tt.lang, tt.admin, err)
		}
		if !strings.Contains(got.HTML, tt.wantSub) {
			t.Errorf("%s admin=%v HTML missing role %q: %q", tt.lang, tt.admin, tt.wantSub, got.HTML)
		}
	}
}

// HTML part must escape user-controlled fields (XSS), while subject/text (plain
// headers/body) must NOT html-escape — proving the text/template vs
// html/template split is wired correctly.
func TestRenderEscapingBoundary(t *testing.T) {
	data := SpaceInviteMemberData{
		InviterName: `<script>alert(1)</script>`,
		SpaceName:   "A & B",
		AcceptURL:   htmltemplate.URL("https://x.test/?token=t"),
	}
	got, err := Render(KeySpaceInviteMember, "en-US", data)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got.HTML, "<script>alert(1)</script>") {
		t.Errorf("HTML did not escape script payload: %q", got.HTML)
	}
	if !strings.Contains(got.HTML, "&lt;script&gt;") {
		t.Errorf("HTML expected escaped script, got: %q", got.HTML)
	}
	// Plaintext alternative keeps the raw ampersand (no &amp;).
	if !strings.Contains(got.Text, "A & B") {
		t.Errorf("Text should keep raw ampersand: %q", got.Text)
	}
}

// AcceptURL is template.URL, so html/template must emit the already-escaped
// query string without re-encoding it (e.g. %2B must NOT become %252B). The
// literal "&" is still entity-escaped to "&amp;" in HTML, which is expected;
// the plaintext part carries the verbatim URL.
func TestRenderAcceptURLNotMangled(t *testing.T) {
	raw := "https://x.test/v1/space/email-invite?token=a%2Bb&lang=zh-CN"
	got, err := Render(KeySpaceInviteMember, "zh-CN", SpaceInviteMemberData{
		SpaceName: "S",
		AcceptURL: htmltemplate.URL(raw),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// %2B preserved (not re-encoded) and "&" entity-escaped in HTML.
	if !strings.Contains(got.HTML, "token=a%2Bb&amp;lang=zh-CN") {
		t.Errorf("HTML mangled accept URL: %q", got.HTML)
	}
	// Plaintext keeps the verbatim URL.
	if !strings.Contains(got.Text, raw) {
		t.Errorf("Text mangled accept URL, want %q in: %q", raw, got.Text)
	}
}

func TestRenderUnknownKey(t *testing.T) {
	if _, err := Render("does_not_exist", "en-US", nil); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

// Unsupported/empty language must fall back rather than error.
func TestRenderLanguageFallback(t *testing.T) {
	for _, lang := range []string{"", "fr-FR", "xx"} {
		got, err := Render(KeyVerifyCode, lang, VerifyCodeData{Code: "000000"})
		if err != nil {
			t.Fatalf("Render lang=%q: %v", lang, err)
		}
		if !strings.Contains(got.HTML, "000000") {
			t.Errorf("lang=%q fallback render missing code: %q", lang, got.HTML)
		}
	}
}

// buildMatrixFS builds an fstest.MapFS containing a complete supported-language
// × expected-key × kind matrix of minimal valid templates, for exercising
// loadFrom's completeness guarantee without touching the shipped embed.
func buildMatrixFS() fstest.MapFS {
	m := fstest.MapFS{}
	for _, lang := range octoi18n.SupportedLanguages() {
		for _, key := range expectedKeys {
			for _, kind := range []string{"subject", "html", "text"} {
				m["templates/"+lang+"/"+key+"."+kind+".tmpl"] = &fstest.MapFile{Data: []byte("x")}
			}
		}
	}
	return m
}

func TestLoadFromCompleteMatrixOK(t *testing.T) {
	if _, err := loadFrom(buildMatrixFS()); err != nil {
		t.Fatalf("complete matrix should load, got: %v", err)
	}
}

// A supported language missing any one part must fail loud at load() — the
// runtime guarantee that lookup()'s source-language fallback is never silently
// exercised for a supported language.
func TestLoadFromIncompleteMatrixFailsLoud(t *testing.T) {
	m := buildMatrixFS()
	var victim string
	for k := range m {
		if strings.HasSuffix(k, "/verify_code.text.tmpl") {
			victim = k
			break
		}
	}
	if victim == "" {
		t.Fatal("test setup: no verify_code.text.tmpl in matrix")
	}
	delete(m, victim)
	if _, err := loadFrom(m); err == nil {
		t.Fatalf("missing %s should fail load, got nil error", victim)
	}
}

// A parse error in any file must surface as a load error, not a panic.
func TestLoadFromParseErrorSurfaces(t *testing.T) {
	m := buildMatrixFS()
	for k := range m {
		if strings.HasSuffix(k, "/verify_code.html.tmpl") {
			m[k] = &fstest.MapFile{Data: []byte("{{.Unclosed")}
			break
		}
	}
	if _, err := loadFrom(m); err == nil {
		t.Fatal("malformed template should fail load, got nil error")
	}
}

// Every supported language must carry a complete template set for every key.
// This is the guard that lets lookup()'s fallback stay defensive-only.
func TestTemplateCompleteness(t *testing.T) {
	loadOnce.Do(load)
	if loadErr != nil {
		t.Fatalf("load: %v", loadErr)
	}
	keys := []string{KeyVerifyCode, KeySpaceInviteOwner, KeySpaceInviteMember}
	for _, lang := range octoi18n.SupportedLanguages() {
		for _, key := range keys {
			cs, ok := compiled[lang+"/"+key]
			if !ok {
				t.Errorf("missing template set %s/%s", lang, key)
				continue
			}
			if cs.subject == nil || cs.html == nil || cs.text == nil {
				t.Errorf("incomplete set %s/%s", lang, key)
			}
		}
	}
}
