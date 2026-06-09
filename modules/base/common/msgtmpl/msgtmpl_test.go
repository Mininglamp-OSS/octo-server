package msgtmpl

import (
	"strings"
	"testing"
	"testing/fstest"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// mapFS builds an in-memory FS from path→content pairs.
func mapFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

// fullCatalogFiles returns a templates tree that defines the same names in
// every supported language, so New succeeds. Each language tags its body so
// tests can tell which tree rendered.
func fullCatalogFiles() map[string]string {
	files := map[string]string{}
	for _, lang := range octoi18n.SupportedLanguages() {
		files["tpl/"+lang+"/greet.tmpl"] = `{{define "greet"}}[` + lang + `] 👋 hi {{.Name}}!{{end}}`
		files["tpl/"+lang+"/bye.tmpl"] = `{{define "bye"}}[` + lang + `] bye{{end}}`
	}
	return files
}

func TestNewAndRenderPerLanguage(t *testing.T) {
	c, err := New(mapFS(fullCatalogFiles()), "tpl")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, lang := range octoi18n.SupportedLanguages() {
		got, err := c.Render("greet", lang, map[string]any{"Name": "Sam"})
		if err != nil {
			t.Fatalf("Render greet/%s: %v", lang, err)
		}
		want := "[" + lang + "] 👋 hi Sam!"
		if got != want {
			t.Fatalf("Render greet/%s = %q, want %q", lang, got, want)
		}
	}
}

func TestNewMissingNameInOneLanguageFailsLoud(t *testing.T) {
	langs := octoi18n.SupportedLanguages()
	if len(langs) < 2 {
		t.Skip("need >=2 supported languages to exercise the matrix gap")
	}
	files := fullCatalogFiles()
	// Drop "bye" from the second language only.
	delete(files, "tpl/"+langs[1]+"/bye.tmpl")

	_, err := New(mapFS(files), "tpl")
	if err == nil || !strings.Contains(err.Error(), `missing message template "bye"`) {
		t.Fatalf("want missing-bye matrix error, got %v", err)
	}
}

func TestNewMissingLanguageDirFailsLoud(t *testing.T) {
	langs := octoi18n.SupportedLanguages()
	files := map[string]string{
		// Only the source language has any templates.
		"tpl/" + octoi18n.SourceLanguage + "/greet.tmpl": `{{define "greet"}}hi{{end}}`,
	}
	_ = langs
	_, err := New(mapFS(files), "tpl")
	if err == nil {
		t.Fatalf("want error when a supported language directory is absent")
	}
}

func TestRenderUnsupportedLanguageFallsBackToSource(t *testing.T) {
	c, err := New(mapFS(fullCatalogFiles()), "tpl")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Render("greet", "fr-FR", map[string]any{"Name": "Sam"})
	if err != nil {
		t.Fatalf("Render greet/fr-FR: %v", err)
	}
	want := "[" + octoi18n.SourceLanguage + "] 👋 hi Sam!"
	if got != want {
		t.Fatalf("fallback render = %q, want source %q", got, want)
	}
}

func TestRenderUnknownNameErrors(t *testing.T) {
	c, err := New(mapFS(fullCatalogFiles()), "tpl")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Render("does-not-exist", octoi18n.SourceLanguage, nil); err == nil {
		t.Fatalf("want error for unknown message name")
	}
}

func TestRenderExecuteErrorPropagates(t *testing.T) {
	files := map[string]string{}
	for _, lang := range octoi18n.SupportedLanguages() {
		// {{.X.Y}} on a struct without an X field is an execution error.
		files["tpl/"+lang+"/boom.tmpl"] = `{{define "boom"}}{{.X.Y}}{{end}}`
	}
	c, err := New(mapFS(files), "tpl")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Render("boom", octoi18n.SourceLanguage, struct{}{}); err == nil {
		t.Fatalf("want execute error for {{.X.Y}} on empty struct")
	}
}

func TestRenderMissingMapKeyErrors(t *testing.T) {
	// missingkey=error must reject a data map that omits a referenced field
	// rather than emit "<no value>". This guards against a call site forgetting
	// a parameter. Critically it must hold for the Lookup()'d {{define}} block,
	// not just the root template (Option is per-*Template and does not propagate).
	files := map[string]string{}
	for _, lang := range octoi18n.SupportedLanguages() {
		files["tpl/"+lang+"/m.tmpl"] = `{{define "m"}}hi {{.Name}}{{end}}`
	}
	c, err := New(mapFS(files), "tpl")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Present key renders fine.
	if got, err := c.Render("m", octoi18n.SourceLanguage, map[string]any{"Name": "x"}); err != nil || got != "hi x" {
		t.Fatalf("Render with key = %q, %v; want %q", got, err, "hi x")
	}
	// Missing key errors.
	if _, err := c.Render("m", octoi18n.SourceLanguage, map[string]any{}); err == nil {
		t.Fatalf("missingkey=error must reject a data map missing .Name")
	}
}

func TestNamesReturnsSortedUnion(t *testing.T) {
	c, err := New(mapFS(fullCatalogFiles()), "tpl")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	names := c.Names()
	if len(names) != 2 || names[0] != "bye" || names[1] != "greet" {
		t.Fatalf("Names() = %v, want [bye greet]", names)
	}
}

func TestMustNewPanicsOnIncompleteCatalog(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("MustNew should panic on an incomplete catalog")
		}
	}()
	MustNew(mapFS(map[string]string{
		"tpl/" + octoi18n.SourceLanguage + "/greet.tmpl": `{{define "greet"}}hi{{end}}`,
	}), "tpl")
}
