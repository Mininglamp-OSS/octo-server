package file

// Exhaustive answer to "which other filenames can still break the presigned
// PUT?" for GH#760.
//
// The 403 happens when the value octo-server signs is not what the storage
// gateway derives from the value the browser sends. Both derivations are
// Trimall over the same string, so the whole failure mode collapses to one
// property:
//
//	the emitted header must be a fixed point of BOTH Trimall rules.
//
// If that holds, minio-go's canonical value, COS/AWS's canonical value, and
// the bytes on the wire are the same string, and no signature mismatch is
// possible — whichever rule the gateway implements.
//
// This file sweeps the filename space against that property instead of
// enumerating suspicious characters, so a future edit that reintroduces raw
// whitespace (or a new header parameter) fails here rather than in
// production.
//
// Both entry shapes are covered, because they differ in the wild:
//
//   - sanitized  — modules/file/api.go runs sanitizeFilename first.
//   - raw        — modules/bot_api/file.go and modules/robot/api.go pass the
//     multipart filename / query parameter straight through.
//
// The raw shape is the adversarial one: control characters and quotes reach
// BuildContentDisposition untouched there.

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
)

// assertHeaderIsSignable checks every property a signed Content-Disposition
// must have for the SigV4 handshake to be deterministic.
func assertHeaderIsSignable(t *testing.T, label, filename string) {
	t.Helper()

	for _, cd := range []string{
		BuildContentDisposition(filename),
		BuildContentDisposition(sanitizeFilename(filename)),
	} {
		if cd == "" {
			continue
		}

		// 1+2. Both canonicalizations must be the identity, so signer and
		// gateway agree no matter which rule the gateway applies.
		if got := trimAllMinIO(cd); got != cd {
			t.Fatalf("%s: minio-go canonicalization rewrites the header\n filename=%q\n emitted =%q\n canon   =%q",
				label, filename, cd, got)
		}
		if got := trimAllQuotedAware(cd); got != cd {
			t.Fatalf("%s: COS/AWS canonicalization rewrites the header\n filename=%q\n emitted =%q\n canon   =%q",
				label, filename, cd, got)
		}

		// 3. The value must be a legal HTTP header value, i.e. no C0 controls
		// and no DEL. This is stricter than "no CR/LF" and it is not implied
		// by property 1: collapsing only folds the whitespace controls, so a
		// 0x01 would survive and Go's transport would refuse to send the
		// request at all ("invalid header field value") — the credentials
		// response would be handing the client a header it cannot use. Only
		// callers that skip sanitizeFilename can get here (bot_api, robot),
		// which is exactly why this is checked on the raw shape too.
		for i := 0; i < len(cd); i++ {
			if c := cd[i]; c < 0x20 || c == 0x7F {
				t.Fatalf("%s: header carries byte 0x%02X, not a valid header value\n filename=%q\n emitted=%q",
					label, c, filename, cd)
			}
		}

		// 4. Quotes stay balanced, so "inside a quoted string" means the same
		// thing to us and to the gateway. Scan RFC 6266 quoted-string rules:
		// inside quotes a backslash escapes the next octet, so `"a\\"` closes
		// (the pair is an escaped backslash) while `"a\""` does not.
		inQuotes := false
		for i := 0; i < len(cd); i++ {
			if inQuotes && cd[i] == '\\' && i+1 < len(cd) {
				i++ // consume the escaped octet
				continue
			}
			if cd[i] == '"' {
				inQuotes = !inQuotes
			}
		}
		if inQuotes {
			t.Fatalf("%s: unbalanced quotes in header\n filename=%q\n emitted=%q", label, filename, cd)
		}
	}
}

// TestIssue760_SweepIsNotVacuous proves the property above actually
// discriminates: the pre-fix construction violates it for the filename from
// the issue. Without this, a change that made BuildContentDisposition return
// something trivially stable (or empty) would sail through every sweep.
func TestIssue760_SweepIsNotVacuous(t *testing.T) {
	pre := naiveContentDisposition(sanitizeFilename("Octo  upload test.pdf"))
	assert.NotEqual(t, pre, trimAllMinIO(pre),
		"pre-fix header must violate the sweep property, else the sweep proves nothing")

	post := BuildContentDisposition(sanitizeFilename("Octo  upload test.pdf"))
	assert.Equal(t, post, trimAllMinIO(post))
	assert.Contains(t, post, "%20%20", "filename* must still carry the user's original spacing")

	// Same for the header-validity property: the unsanitized shape used by
	// bot_api / robot really can carry a C0 control, so property 3 is not
	// decoration either.
	preCtl := naiveContentDisposition("a\x01b.pdf")
	assert.Contains(t, preCtl, "\x01",
		"pre-fix header must carry the raw control byte, else property 3 proves nothing")

	postCtl := BuildContentDisposition("a\x01b.pdf")
	assert.NotContains(t, postCtl, "\x01")
	assert.Equal(t, `inline; filename="a_b.pdf"; filename*=UTF-8''a%01b.pdf`, postCtl,
		"control byte is scrubbed from the quoted fallback but preserved, percent-encoded, in filename*")
}

// TestIssue760_SweepASCII places every ASCII code point — control characters,
// quotes, backslashes, delimiters — at the start, middle and end of a
// filename, singly and in runs.
func TestIssue760_SweepASCII(t *testing.T) {
	for c := 0; c < 128; c++ {
		r := rune(c)
		for _, n := range []int{1, 2, 3} {
			run := strings.Repeat(string(r), n)
			for _, shape := range []string{run + "name.pdf", "na" + run + "me.pdf", "name" + run + ".pdf", "name.pdf" + run} {
				assertHeaderIsSignable(t, fmt.Sprintf("ascii U+%04X x%d", c, n), shape)
			}
		}
	}
}

// TestIssue760_SweepUnicodeWhitespace covers every code point Go considers
// whitespace — the set strings.Fields actually splits on. U+0085 and U+00A0
// are the interesting ones: they are whitespace to unicode.IsSpace but are
// non-ASCII, so they take the '_' fallback branch rather than surviving as
// literal whitespace.
func TestIssue760_SweepUnicodeWhitespace(t *testing.T) {
	var spaces []rune
	for _, rg := range unicode.White_Space.R16 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			spaces = append(spaces, r)
		}
	}
	for _, rg := range unicode.White_Space.R32 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			spaces = append(spaces, r)
		}
	}
	assert.NotEmpty(t, spaces)
	t.Logf("sweeping %d whitespace code points", len(spaces))

	for _, r := range spaces {
		for _, n := range []int{1, 2, 3} {
			run := strings.Repeat(string(r), n)
			for _, shape := range []string{
				run + "name.pdf",
				"na" + run + "me.pdf",
				"报告" + run + "文档.pdf",
				"name" + run + ".pdf",
				// Mixed runs: an ASCII space next to an exotic one is the
				// case a naive "replace two spaces with one" fix would miss.
				"na " + run + " me.pdf",
			} {
				assertHeaderIsSignable(t, fmt.Sprintf("U+%04X x%d", r, n), shape)
			}
		}
	}
}

// TestIssue760_SweepAdversarial covers hand-picked shapes aimed at the header
// grammar rather than at whitespace: quote and backslash injection, fake
// parameters, RFC 5987 look-alikes, and names that collapse to nothing.
func TestIssue760_SweepAdversarial(t *testing.T) {
	names := []string{
		``,
		` `,
		`   `,
		"\t\t",
		"\r\n",
		"a\r\nX-Injected: 1.pdf",
		`a"b.pdf`,
		`a""b.pdf`,
		`a\"b.pdf`,
		`a\\b.pdf`,
		`a\.pdf`,
		`"; filename="evil.pdf`,
		`a"; filename*=UTF-8''evil.pdf`,
		`report.pdf"; x="  y`,
		`inline; filename="x  y.pdf"`,
		`filename*=UTF-8''a%20%20b.pdf`,
		`a;b.pdf`,
		`a,b.pdf`,
		`../../etc/passwd`,
		`..\..\windows\system32.dll`,
		strings.Repeat("a b", 200) + ".pdf",
		strings.Repeat(" ", 300) + ".pdf",
		strings.Repeat("报告 ", 200) + ".pdf",
		"emoji\U0001F600\U0001F600  \U0001F600.pdf",
	}
	for _, n := range names {
		assertHeaderIsSignable(t, fmt.Sprintf("adversarial %q", n), n)
	}
}

// TestIssue760_SweepRandom fuzzes over an alphabet weighted towards the
// characters that matter — whitespace, quotes, backslashes, delimiters, CJK,
// emoji — with a fixed seed so a failure is reproducible.
func TestIssue760_SweepRandom(t *testing.T) {
	alphabet := []rune{
		' ', ' ', ' ', '\t', '\n', '\r', '\v', '\f',
		'"', '\\', ';', '=', ',', '\'', '*', '/', '.', '-', '_',
		'a', 'B', '7', 0x00, 0x1F, 0x7F,
		'', ' ', ' ', ' ', ' ', ' ', ' ', '　', '​',
		'报', '告', '\U0001F600',
	}
	rng := rand.New(rand.NewSource(760))

	const cases = 20000
	for i := 0; i < cases; i++ {
		n := 1 + rng.Intn(24)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		name := b.String() + ".pdf"
		assertHeaderIsSignable(t, fmt.Sprintf("random#%d", i), name)
	}
	t.Logf("swept %d random filenames with no destabilizing header", cases)
}
