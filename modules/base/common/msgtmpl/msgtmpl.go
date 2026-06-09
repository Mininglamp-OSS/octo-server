// Package msgtmpl renders outbound IM / push message bodies from per-language
// template sets embedded at build time.
//
// It is the i18n surface for *outbound message content* sent to users over
// WuKongIM (BotFather DM replies, webhook push text, ...), parallel to — but
// deliberately independent of — two neighbouring mechanisms:
//
//   - pkg/i18n's error-code localizer (codes.Register + Localizer.Translate)
//     carries single-line, parameterized *error* messages keyed by
//     `err.(shared|server).*` and wrapped in the HTTP error envelope. Outbound
//     IM content is not an error response and must not go through that envelope.
//   - modules/base/common/emailtmpl renders the three-part email shape
//     (subject + HTML + text). Outbound IM content is a single plain body, so
//     it uses its own loader rather than being forced through that 3-file shape.
//
// A Catalog is a namespace-agnostic mechanism: each caller (BotFather, push,
// ...) embeds its own templates/{lang}/*.tmpl tree and constructs one Catalog
// over it, so a single shared mechanism backs every outbound-content surface
// instead of N parallel designs.
//
// Each language's templates live under {root}/{lang}/ and may be split across
// any number of *.tmpl files. A logical message is a named block:
//
//	{{define "welcome"}}👋 Hello {{.Name}}{{end}}
//
// Splitting by domain (command.tmpl, apply.tmpl, ...) keeps long multi-line
// Markdown bodies readable and diffable while still compiling, per language,
// into a single text/template tree. text/template (not html/template) is used
// on purpose: IM bodies are Markdown/plain text delivered to a chat client, not
// HTML, so html-escaping "A & B" → "A &amp; B" would corrupt them.
package msgtmpl

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"text/template"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// Catalog holds one compiled text/template tree per supported language. It is
// immutable after New and safe for concurrent Render calls.
type Catalog struct {
	byLang map[string]*template.Template // key: supported language tag
	names  []string                      // sorted union of defined message names
}

// New loads and compiles the template tree under root for every supported
// language, then enforces the completeness matrix: every supported language
// must define exactly the same set of message names. A parse error, an empty
// language directory, or a name present in one language but missing in another
// returns an error — fail-loud at construction rather than rendering a missing
// body (or silently falling back to the source language) at runtime.
//
// fsys is a parameter (a //go:embed FS in production) so tests can inject an
// fstest.MapFS to exercise the completeness guarantee without shipping fixtures.
func New(fsys fs.FS, root string) (*Catalog, error) {
	langs := octoi18n.SupportedLanguages()
	if len(langs) == 0 {
		return nil, fmt.Errorf("msgtmpl: no supported languages configured")
	}

	byLang := make(map[string]*template.Template, len(langs))
	nameSets := make(map[string]map[string]struct{}, len(langs))
	union := map[string]struct{}{}

	for _, lang := range langs {
		pattern := path.Join(root, lang, "*.tmpl")
		t, err := template.New(lang).ParseFS(fsys, pattern)
		if err != nil {
			return nil, fmt.Errorf("msgtmpl: load language %s (pattern %q): %w", lang, pattern, err)
		}
		// missingkey=error turns a "{{.Field}} not in the data map" miss into a
		// hard render error instead of a silently emitted "<no value>", so a call
		// site that forgets a parameter fails loudly (and replyL/localizedMessage
		// log + skip) rather than DMing a half-built message. Option does NOT
		// propagate from the root template to the Lookup()'d {{define}} blocks
		// (it is per-*Template), so it must be set on every associated template.
		for _, assoc := range t.Templates() {
			assoc.Option("missingkey=error")
		}
		set := definedNames(t, lang)
		if len(set) == 0 {
			return nil, fmt.Errorf("msgtmpl: language %s defines no message templates under %s", lang, path.Join(root, lang))
		}
		byLang[lang] = t
		nameSets[lang] = set
		for n := range set {
			union[n] = struct{}{}
		}
	}

	// Completeness matrix: every supported language defines every name in the
	// union. This is the runtime guarantee that lets Render stay defensive-only.
	for _, lang := range langs {
		set := nameSets[lang]
		for n := range union {
			if _, ok := set[n]; !ok {
				return nil, fmt.Errorf("msgtmpl: language %s missing message template %q", lang, n)
			}
		}
	}

	if _, ok := byLang[octoi18n.SourceLanguage]; !ok {
		return nil, fmt.Errorf("msgtmpl: source language %s not present in supported set", octoi18n.SourceLanguage)
	}

	names := make([]string, 0, len(union))
	for n := range union {
		names = append(names, n)
	}
	sort.Strings(names)

	return &Catalog{byLang: byLang, names: names}, nil
}

// MustNew is New but panics on error. Intended for package-level singletons fed
// by a //go:embed FS, where a failure is a build/asset defect surfaced at
// startup, not a recoverable runtime condition.
func MustNew(fsys fs.FS, root string) *Catalog {
	c, err := New(fsys, root)
	if err != nil {
		panic(err)
	}
	return c
}

// Render executes the named message in the requested language and returns the
// rendered body. lang is normalized to the supported matrix; an unsupported or
// empty language falls back to the source language. An unknown message name
// returns an error (a caller bug — the name must be one defined in the tree).
func (c *Catalog) Render(name, lang string, data any) (string, error) {
	t := c.treeFor(lang)
	tt := t.Lookup(name)
	if tt == nil {
		return "", fmt.Errorf("msgtmpl: unknown message template %q (lang %q)", name, lang)
	}
	var buf bytes.Buffer
	if err := tt.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("msgtmpl: execute %q (lang %q): %w", name, lang, err)
	}
	return buf.String(), nil
}

// Names returns the sorted union of defined message names. Useful for
// completeness tests that assert every caller-declared key renders in every
// supported language.
func (c *Catalog) Names() []string {
	out := make([]string, len(c.names))
	copy(out, c.names)
	return out
}

// treeFor resolves the template tree for lang, falling back to the source
// language for an unsupported/empty tag. Never returns nil: New guarantees the
// source language is present.
func (c *Catalog) treeFor(lang string) *template.Template {
	if norm, ok := octoi18n.MatchSupportedLanguage(lang); ok {
		if t := c.byLang[norm]; t != nil {
			return t
		}
	}
	return c.byLang[octoi18n.SourceLanguage]
}

// definedNames returns the set of {{define "name"}} block names in t, excluding
// the root template (named rootName) and the per-file templates ParseFS creates
// (named "{file}.tmpl"). What remains is exactly the logical message names.
func definedNames(t *template.Template, rootName string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tt := range t.Templates() {
		n := tt.Name()
		if n == "" || n == rootName || strings.HasSuffix(n, ".tmpl") {
			continue
		}
		if tt.Tree == nil {
			continue
		}
		out[n] = struct{}{}
	}
	return out
}
