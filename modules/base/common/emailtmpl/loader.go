// Package emailtmpl renders outbound transactional email (subject + HTML +
// plaintext) from per-language templates embedded at build time.
//
// It is the i18n surface for email *bodies*, parallel to — but deliberately
// independent of — pkg/i18n's error-code localizer. The code localizer
// (codes.Register + Localizer.Translate) carries single-line, parameterized
// error messages keyed by `err.(shared|server).*`; multi-line HTML email is a
// different shape and intentionally uses its own embedded template tree rather
// than being forced through that registry.
//
// Each logical email is three sibling files under templates/{lang}/:
//
//	{key}.subject.tmpl  — text/template (plain header, NOT html-escaped)
//	{key}.html.tmpl     — html/template (auto-escaped, XSS-safe)
//	{key}.text.tmpl     — text/template (plaintext alternative part)
//
// subject/text use text/template and html uses html/template on purpose:
// rendering a Subject header through html/template would turn "A & B" into
// "A &amp; B"; rendering HTML through text/template would drop XSS escaping on
// user-controlled fields (inviter name, space name, ...).
package emailtmpl

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"strings"
	"sync"
	texttemplate "text/template"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

//go:embed templates
var templatesFS embed.FS

// Message keys — one per logical email. Kept as constants so call sites and
// the template tree cannot silently drift.
const (
	KeyVerifyCode        = "verify_code"
	KeySpaceInviteOwner  = "space_invite_owner"
	KeySpaceInviteMember = "space_invite_member"
)

// fallbackLanguage is the source language (en-US), treated as the canonical
// complete template set. lookup() falls back to it only for an *unsupported*
// requested language — every *supported* language is guaranteed complete by the
// load()-time matrix check (see expectedKeys), so a supported language never
// reaches this fallback.
//
// This is intentionally the source language, NOT the runtime
// OCTO_DEFAULT_LANGUAGE (which may be zh-CN). emailtmpl's contract is "the
// source language is always fully present"; a missing piece for a *supported*
// language fails loud at load() rather than silently rendering en-US bodies
// under a zh-CN-default deployment.
const fallbackLanguage = octoi18n.SourceLanguage // "en-US"

// expectedKeys is the message-key matrix every supported language must fully
// provide. Kept explicit (not derived from whatever files happen to exist) so a
// supported language shipped with zero files is caught at load() time rather
// than silently falling back to the source language at render time.
var expectedKeys = []string{KeyVerifyCode, KeySpaceInviteOwner, KeySpaceInviteMember}

// Rendered is the output of Render: the three parts a transactional email
// needs. Subject is trimmed (no stray trailing newline leaking into the SMTP
// header); HTML and Text are emitted verbatim.
type Rendered struct {
	Subject string
	HTML    string
	Text    string
}

// VerifyCodeData drives the verify_code template.
type VerifyCodeData struct {
	Code string
}

// SpaceInviteOwnerData drives the space_invite_owner template. An empty
// InviterName is handled inside the template (localized "Octo admin" fallback)
// so the fallback text itself is translated rather than hardcoded in Go.
// AcceptURL is template.URL so html/template treats the already-escaped link as
// a safe URL instead of re-filtering it.
type SpaceInviteOwnerData struct {
	InviterName string
	PlannedName string
	PlannedDesc string
	AcceptURL   htmltemplate.URL
}

// SpaceInviteMemberData drives the space_invite_member template. IsAdmin
// selects the localized role label inside the template (was a hardcoded
// "成员"/"管理员" branch in Go).
type SpaceInviteMemberData struct {
	InviterName string
	SpaceName   string
	IsAdmin     bool
	AcceptURL   htmltemplate.URL
}

type compiledSet struct {
	subject *texttemplate.Template
	html    *htmltemplate.Template
	text    *texttemplate.Template
}

var (
	loadOnce sync.Once
	compiled map[string]*compiledSet // key: "{lang}/{msgKey}"
	loadErr  error
)

// Render produces the subject/HTML/text for a message key in the requested
// language. lang is normalized to the supported matrix; an unsupported/missing
// language falls back to fallbackLanguage. Call sites that have no per-recipient
// signal should pass i18n.OutboundLanguage(ctx), which already resolves to
// OCTO_DEFAULT_LANGUAGE.
func Render(key, lang string, data any) (Rendered, error) {
	loadOnce.Do(load)
	if loadErr != nil {
		return Rendered{}, loadErr
	}
	cs, err := lookup(key, lang)
	if err != nil {
		return Rendered{}, err
	}
	subject, err := execText(cs.subject, data)
	if err != nil {
		return Rendered{}, fmt.Errorf("emailtmpl: render subject %s/%s: %w", lang, key, err)
	}
	html, err := execHTML(cs.html, data)
	if err != nil {
		return Rendered{}, fmt.Errorf("emailtmpl: render html %s/%s: %w", lang, key, err)
	}
	text, err := execText(cs.text, data)
	if err != nil {
		return Rendered{}, fmt.Errorf("emailtmpl: render text %s/%s: %w", lang, key, err)
	}
	return Rendered{
		Subject: strings.TrimSpace(subject),
		HTML:    html,
		Text:    text,
	}, nil
}

func lookup(key, lang string) (*compiledSet, error) {
	if norm, ok := octoi18n.MatchSupportedLanguage(lang); ok {
		lang = norm
	}
	if cs, ok := compiled[lang+"/"+key]; ok {
		return cs, nil
	}
	if cs, ok := compiled[fallbackLanguage+"/"+key]; ok {
		return cs, nil
	}
	return nil, fmt.Errorf("emailtmpl: no template for key=%q lang=%q", key, lang)
}

func execText(t *texttemplate.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func execHTML(t *htmltemplate.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// load compiles the embedded template tree once into the package `compiled`
// map; the result/error are surfaced on the first Render call.
func load() {
	compiled, loadErr = loadFrom(templatesFS)
}

// loadFrom walks fsys, compiles every {key}.{kind}.tmpl file, and then enforces
// the declared completeness matrix (every supported language × every
// expectedKey, all three kinds present). A parse error, a stray file, or a
// missing/partial set for a supported language returns an error — fail-loud
// rather than rendering a half-built email (or silently falling back to the
// source language) at runtime.
//
// fsys is a parameter (defaulting to the embed in load()) so tests can inject an
// fstest.MapFS to exercise the incomplete-set guarantee without mutating the
// shipped templates.
func loadFrom(fsys fs.FS) (map[string]*compiledSet, error) {
	sets := map[string]*compiledSet{}
	walkErr := fs.WalkDir(fsys, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		// path: templates/{lang}/{key}.{kind}.tmpl
		rel := strings.TrimPrefix(path, "templates/")
		lang, file, ok := strings.Cut(rel, "/")
		if !ok {
			return fmt.Errorf("emailtmpl: unexpected template path %q", path)
		}
		name := strings.TrimSuffix(file, ".tmpl")
		key, kind, ok := cutLast(name, ".")
		if !ok {
			return fmt.Errorf("emailtmpl: template %q must be {key}.{kind}.tmpl", path)
		}
		data, rerr := fs.ReadFile(fsys, path)
		if rerr != nil {
			return rerr
		}
		setKey := lang + "/" + key
		cs := sets[setKey]
		if cs == nil {
			cs = &compiledSet{}
			sets[setKey] = cs
		}
		switch kind {
		case "subject":
			t, perr := texttemplate.New(setKey + ".subject").Parse(string(data))
			if perr != nil {
				return fmt.Errorf("emailtmpl: parse %s: %w", path, perr)
			}
			cs.subject = t
		case "html":
			t, perr := htmltemplate.New(setKey + ".html").Parse(string(data))
			if perr != nil {
				return fmt.Errorf("emailtmpl: parse %s: %w", path, perr)
			}
			cs.html = t
		case "text":
			t, perr := texttemplate.New(setKey + ".text").Parse(string(data))
			if perr != nil {
				return fmt.Errorf("emailtmpl: parse %s: %w", path, perr)
			}
			cs.text = t
		default:
			return fmt.Errorf("emailtmpl: unknown template kind %q in %s", kind, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	// Declared-matrix completeness: every supported language must fully provide
	// every expected key. This is the runtime guarantee that lets lookup()'s
	// source-language fallback stay defensive-only for supported languages.
	for _, lang := range octoi18n.SupportedLanguages() {
		for _, key := range expectedKeys {
			cs, ok := sets[lang+"/"+key]
			if !ok || cs.subject == nil || cs.html == nil || cs.text == nil {
				return nil, fmt.Errorf("emailtmpl: missing or incomplete template set %s/%s", lang, key)
			}
		}
	}
	return sets, nil
}

// cutLast splits name on the last occurrence of sep ("verify_code.subject" →
// "verify_code", "subject"). Needed because message keys may contain no dots
// today but kinds always sit after the final dot.
func cutLast(name, sep string) (before, after string, found bool) {
	i := strings.LastIndex(name, sep)
	if i < 0 {
		return name, "", false
	}
	return name[:i], name[i+len(sep):], true
}
