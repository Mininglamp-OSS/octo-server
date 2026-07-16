package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFixture(t *testing.T, dir, name, source string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600))
}

// writeFixtureIn writes into dir/subdir, creating it, so a fixture package can
// live at a directory path the allowlist can anchor on.
func writeFixtureIn(t *testing.T, dir, subdir, name, source string) {
	t.Helper()
	full := filepath.Join(dir, subdir)
	require.NoError(t, os.MkdirAll(full, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(full, name), []byte(source), 0o600))
}

func TestScanRejectsInternalCardTransportBypasses(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "direct SendMessage literal",
			files: map[string]string{"bypass.go": `package bad
func bypass() { payload := map[string]interface{}{"type": 17}; ctx.SendMessage(payload) }
`},
		},
		{
			name: "direct SendMessageWithResult constant",
			files: map[string]string{"bypass.go": `package bad
func bypass() { payload := map[string]interface{}{"type": cardmsg.InteractiveCard.Int()}; ctx.SendMessageWithResult(payload) }
`},
		},
		{
			name: "direct SendMessageBatch",
			files: map[string]string{"bypass.go": `package bad
func bypass() { payload := map[string]interface{}{"type": 17}; ctx.SendMessageBatch(payload) }
`},
		},
		{
			name: "package local transport wrapper",
			files: map[string]string{"bypass.go": `package bad
func dispatch(v interface{}) { ctx.SendMessage(v) }
func bypass() { payload := map[string]interface{}{"type": 17}; dispatch(payload) }
`},
		},
		{
			name: "construction and transport split across files",
			files: map[string]string{
				"card.go": `package bad
func makeCard() interface{} { return map[string]interface{}{"type": 17} }
`,
				"send.go": `package bad
func dispatch(v interface{}) { ctx.SendMessageWithResult(v) }
func bypass() { dispatch(makeCard()) }
`,
			},
		},
		{
			name: "local constant card type",
			files: map[string]string{"bypass.go": `package bad
func bypass() {
	const interactiveCard = 17
	payload := map[string]interface{}{"type": interactiveCard}
	ctx.SendMessage(payload)
}
`},
		},
		{
			name: "card type assigned after map construction",
			files: map[string]string{"bypass.go": `package bad
func bypass() {
	payload := map[string]interface{}{}
	payload["type"] = 17
	ctx.SendMessageWithResult(payload)
}
`},
		},
		{
			name: "transport method alias",
			files: map[string]string{"bypass.go": `package bad
func bypass() {
	send := ctx.SendMessage
	payload := map[string]interface{}{"type": 17}
	send(payload)
}
`},
		},
		{
			name: "package local wrapper alias",
			files: map[string]string{"bypass.go": `package bad
func dispatch(v interface{}) { ctx.SendMessage(v) }
func bypass() {
	send := dispatch
	payload := map[string]interface{}{"type": 17}
	send(payload)
}
`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, source := range tc.files {
				writeFixture(t, dir, name, source)
			}
			findings, err := Scan([]string{dir}, nil)
			require.NoError(t, err)
			require.Len(t, findings, 1)
			assert.Contains(t, findings[0].Function, "bypass")
		})
	}
}

func TestScanAllowlistIsExactFunctionNotPackageWide(t *testing.T) {
	dir := t.TempDir()
	writeFixtureIn(t, dir, "bot_api", "send.go", `package bot_api
func sendMessage() { payload := map[string]interface{}{"type": 17}; ctx.SendMessageWithResult(payload) }
func secondBypass() { payload := map[string]interface{}{"type": 17}; ctx.SendMessageWithResult(payload) }
`)
	allow := []AllowlistEntry{{Path: "bot_api", Function: "sendMessage", Reason: "reviewed external ingress"}}
	findings, err := Scan([]string{dir}, allow)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "secondBypass", findings[0].Function)
}

// F2: an impostor directory that merely reuses an allowlisted package name +
// receiver.method is NOT exempt, because the allowlist anchors on the path.
func TestScanAllowlistDoesNotExemptImpostorPackageName(t *testing.T) {
	dir := t.TempDir()
	writeFixtureIn(t, dir, "evil", "send.go", `package carddispatch
type producerSender struct{}
func (s *producerSender) Send() { payload := map[string]interface{}{"type": 17}; ctx.SendMessageWithResult(payload) }
`)
	findings, err := Scan([]string{dir}, defaultAllowlist)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "Send", findings[0].Function)
	assert.Equal(t, "producerSender", findings[0].Receiver)
}

func TestScanDoesNotTreatCardRejectionAsConstruction(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "gate.go", `package notify
func deliver(payload map[string]interface{}) {
	if cardmsg.IsCardPayload(payload) { return }
	ctx.SendMessage(map[string]interface{}{"type": 1, "content": "text"})
}
`)

	findings, err := Scan([]string{dir}, nil)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestScanStopsPropagationAtAllowlistedProducer(t *testing.T) {
	dir := t.TempDir()
	writeFixtureIn(t, dir, "ingress", "producer.go", `package ingress
func handlePush() {
	payload := map[string]interface{}{"type": 17}
	ctx.SendMessageWithResult(payload)
}
func route() { handlePush() }
`)
	allow := []AllowlistEntry{{Path: "ingress", Function: "handlePush", Reason: "reviewed ingress"}}

	findings, err := Scan([]string{dir}, allow)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

// F1: a transport call reached through a method/field/higher-order value —
// not the enclosing receiver — must still be caught.
func TestScanRejectsWrapperTransportEvasions(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "extract-method wrapper on package-local value",
			files: map[string]string{"bypass.go": `package bad
type sender struct{}
func (s *sender) go2(v interface{}) { ctx.SendMessage(v) }
func bypass() {
	s := sender{}
	payload := map[string]interface{}{"type": 17}
	s.go2(payload)
}
`},
		},
		{
			name: "extract-method wrapper via pointer value",
			files: map[string]string{"bypass.go": `package bad
type sender struct{}
func (s *sender) go2(v interface{}) { ctx.SendMessage(v) }
func bypass() {
	s := &sender{}
	payload := map[string]interface{}{"type": 17}
	s.go2(payload)
}
`},
		},
		{
			name: "transport stored in a struct field",
			files: map[string]string{"bypass.go": `package bad
type box struct{ fn func(interface{}) }
func bypass() {
	b := box{fn: ctx.SendMessage}
	payload := map[string]interface{}{"type": 17}
	b.fn(payload)
}
`},
		},
		{
			name: "transport passed as a higher-order argument",
			files: map[string]string{"bypass.go": `package bad
func run(fn func(interface{}), v interface{}) { fn(v) }
func bypass() {
	payload := map[string]interface{}{"type": 17}
	run(ctx.SendMessageWithResult, payload)
}
`},
		},
		{
			name: "construction and transport split across receiver methods",
			files: map[string]string{"bypass.go": `package bad
type maker struct{}
func (m *maker) build() interface{} { return map[string]interface{}{"type": 17} }
type pusher struct{}
func (s *pusher) push(v interface{}) { ctx.SendMessage(v) }
func bypass() {
	m := maker{}
	s := pusher{}
	s.push(m.build())
}
`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, source := range tc.files {
				writeFixture(t, dir, name, source)
			}
			findings, err := Scan([]string{dir}, nil)
			require.NoError(t, err)
			require.Len(t, findings, 1)
			assert.Contains(t, findings[0].Function, "bypass")
		})
	}
}

func TestScanSeparatesMethodsByReceiver(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "receivers.go", `package clean
type builder struct{}
func (b *builder) step() { _ = map[string]interface{}{"type": 17} }
func (b *builder) route() { b.step() }

type transport struct{}
func (s *transport) step() { ctx.SendMessage(nil) }
`)

	findings, err := Scan([]string{dir}, nil)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestDefaultAllowlistHasReasonsAndRepositoryPasses(t *testing.T) {
	for _, entry := range defaultAllowlist {
		assert.NotEmpty(t, entry.Path)
		assert.NotEmpty(t, entry.Function)
		assert.NotEmpty(t, entry.Reason)
	}
	findings, err := Scan([]string{"../../modules", "../../internal"}, defaultAllowlist)
	require.NoError(t, err)
	assert.Empty(t, findings)
}
