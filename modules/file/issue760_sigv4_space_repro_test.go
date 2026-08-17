package file

// Local end-to-end reproduction for GH#760 —
// "文件名含连续半角空格时预签名 PUT 稳定 403 SignatureDoesNotMatch".
//
// This test needs no cloud credentials and no network. It wires up the exact
// production pieces:
//
//   - sanitizeFilename        (const.go, as called by getUploadCredentials)
//   - BuildContentDisposition (api.go)
//   - minio-go v7.0.61 PresignHeader with the same signed header set as
//     ServiceCOS.PresignedPutURL (Content-Length / Content-Type /
//     Content-Disposition)
//
// ...and points the presigned PUT at a local httptest server that verifies
// SigV4 itself. The verifier is parameterised by the ONE thing the two
// implementations disagree on: how a signed header value is "Trimall"-ed.
//
//	minio-go  : strings.Fields  → collapses whitespace runs EVERYWHERE,
//	            including inside a quoted string.
//	AWS / COS : collapses runs OUTSIDE quoted strings only. Per
//	            https://docs.aws.amazon.com/general/latest/gr/sigv4-create-canonical-request.html
//	            "Do not trim excess white space in a header value if it
//	            appears within a quoted string."
//
// Content-Disposition is emitted as `inline; filename="<name>"; filename*=...`,
// so a filename carrying two or more consecutive U+0020 puts a whitespace run
// inside the quoted string — the one place the two rules differ.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	repro760AccessKey = "AKIAREPRO760EXAMPLE"
	repro760SecretKey = "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY"
	repro760Region    = "ap-guangzhou"
	repro760Bucket    = "octo-repro-1250000000"
)

// ---------------------------------------------------------------------------
// The two Trimall implementations
// ---------------------------------------------------------------------------

// trimAllMinIO is a verbatim copy of minio-go v7.0.61
// pkg/signer/utils.go signV4TrimAll — what octo-server signs with today.
func trimAllMinIO(input string) string {
	return strings.Join(strings.Fields(input), " ")
}

// trimAllQuotedAware collapses whitespace runs outside quoted strings and
// leaves whitespace inside quoted strings untouched — the AWS SigV4 rule
// that Tencent COS enforces on the verifying side.
func trimAllQuotedAware(input string) string {
	input = strings.Trim(input, " \t")
	var b strings.Builder
	inQuotes := false
	prevSpace := false
	for i := 0; i < len(input); i++ {
		c := input[i]
		if c == '\\' && inQuotes && i+1 < len(input) {
			b.WriteByte(c)
			b.WriteByte(input[i+1])
			i++
			prevSpace = false
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			b.WriteByte(c)
			prevSpace = false
			continue
		}
		if !inQuotes && (c == ' ' || c == '\t') {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteByte(' ')
			continue
		}
		prevSpace = false
		b.WriteByte(c)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Minimal SigV4 presigned-PUT verifier, standing in for the storage gateway
// ---------------------------------------------------------------------------

type repro760Gateway struct {
	trimAll func(string) string

	lastCanonicalRequest string
	lastStatus           int
}

func (g *repro760Gateway) canonicalRequest(r *http.Request) string {
	q := r.URL.Query()
	signedHeaders := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	sort.Strings(signedHeaders)

	q.Del("X-Amz-Signature")
	canonicalQuery := strings.ReplaceAll(q.Encode(), "+", "%20")

	var headers strings.Builder
	for _, h := range signedHeaders {
		var value string
		switch h {
		case "host":
			value = r.Host
		case "content-length":
			// net/http hoists Content-Length out of the header map.
			value = strconv.FormatInt(r.ContentLength, 10)
		default:
			value = r.Header.Get(h)
		}
		headers.WriteString(h)
		headers.WriteByte(':')
		headers.WriteString(g.trimAll(value))
		headers.WriteByte('\n')
	}

	return strings.Join([]string{
		r.Method,
		s3utils.EncodePath(r.URL.Path),
		canonicalQuery,
		headers.String(),
		strings.Join(signedHeaders, ";"),
		"UNSIGNED-PAYLOAD",
	}, "\n")
}

func (g *repro760Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)

	q := r.URL.Query()
	credential := q.Get("X-Amz-Credential")
	parts := strings.SplitN(credential, "/", 2)
	if len(parts) != 2 {
		g.lastStatus = http.StatusForbidden
		http.Error(w, "InvalidCredential", http.StatusForbidden)
		return
	}
	scope := parts[1]
	scopeDate := strings.SplitN(scope, "/", 2)[0]

	canonicalReq := g.canonicalRequest(r)
	g.lastCanonicalRequest = canonicalReq

	sum := sha256.Sum256([]byte(canonicalReq))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		q.Get("X-Amz-Date"),
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	mac := func(key, data []byte) []byte {
		h := hmac.New(sha256.New, key)
		h.Write(data)
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+repro760SecretKey), []byte(scopeDate))
	k = mac(k, []byte(repro760Region))
	k = mac(k, []byte("s3"))
	k = mac(k, []byte("aws4_request"))
	expected := hex.EncodeToString(mac(k, []byte(stringToSign)))

	if !hmac.Equal([]byte(expected), []byte(q.Get("X-Amz-Signature"))) {
		g.lastStatus = http.StatusForbidden
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SignatureDoesNotMatch</Code></Error>`)
		return
	}
	g.lastStatus = http.StatusOK
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Production signing path (mirrors ServiceCOS.PresignedPutURL)
// ---------------------------------------------------------------------------

func repro760Presign(t *testing.T, host string, contentType, contentDisposition string, fileSize int64) *url.URL {
	t.Helper()

	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(repro760AccessKey, repro760SecretKey, ""),
		Secure:       false, // httptest is plain HTTP; signing math is identical
		Region:       repro760Region,
		BucketLookup: minio.BucketLookupPath,
	})
	require.NoError(t, err)

	headers := http.Header{}
	headers.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if contentDisposition != "" {
		headers.Set("Content-Disposition", contentDisposition)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	presigned, err := client.PresignHeader(ctx, http.MethodPut,
		repro760Bucket, "chat/1755000000/repro/object.bin", 30*time.Minute, nil, headers)
	require.NoError(t, err)
	return presigned
}

// repro760Upload runs the full client flow for one filename: sanitize →
// BuildContentDisposition → presign → PUT with the echoed headers.
func repro760Upload(t *testing.T, gw *repro760Gateway, srv *httptest.Server, rawFilename string) (status int, disposition string) {
	t.Helper()

	filename := sanitizeFilename(rawFilename)
	disposition = BuildContentDisposition(filename)
	body := bytes.Repeat([]byte{0x41}, 4096)
	contentType := "application/octet-stream"

	presigned := repro760Presign(t, srv.Listener.Addr().String(), contentType, disposition, int64(len(body)))

	req, err := http.NewRequest(http.MethodPut, presigned.String(), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Disposition", disposition)
	req.ContentLength = int64(len(body))

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	_ = gw
	return resp.StatusCode, disposition
}

func repro760NewServer(t *testing.T, trimAll func(string) string) (*httptest.Server, *repro760Gateway) {
	t.Helper()
	gw := &repro760Gateway{trimAll: trimAll}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, gw
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// wantStatusFor states the law this whole file is about: a presigned PUT is
// accepted by a COS/AWS-style gateway if and only if the signed header value
// is already stable under Trimall. It holds before the fix (double-space
// filenames are unstable → 403) and after it (all filenames stable → 200),
// so these tests never have to be rewritten alongside the fix.
func wantStatusFor(contentDisposition string) int {
	if trimAllMinIO(contentDisposition) == contentDisposition {
		return http.StatusOK
	}
	return http.StatusForbidden
}

// TestIssue760_ConsecutiveSpacesBreakPresignedPUT is the core reproduction:
// same bytes, same code path, filename differing only in the number of
// consecutive U+0020, against a gateway that applies the AWS quoted-string
// Trimall rule.
//
// Pre-fix this logs `double space → PUT 403` — GH#760 reproduced offline.
// Post-fix BuildContentDisposition emits a Trimall-stable value and the same
// case logs 200; the assertion below tracks either state.
func TestIssue760_ConsecutiveSpacesBreakPresignedPUT(t *testing.T) {
	srv, gw := repro760NewServer(t, trimAllQuotedAware) // COS-side behaviour

	okStatus, okCD := repro760Upload(t, gw, srv, "Octo upload test.pdf")
	t.Logf("single space  → PUT %d  CD=%q", okStatus, okCD)
	assert.Equal(t, http.StatusOK, okStatus, "single space must upload fine")

	badStatus, badCD := repro760Upload(t, gw, srv, "Octo  upload test.pdf")
	t.Logf("double space  → PUT %d  CD=%q", badStatus, badCD)
	assert.Equal(t, wantStatusFor(badCD), badStatus,
		"PUT outcome must track Trimall stability of the signed header")

	if badStatus == http.StatusForbidden {
		t.Logf("GH#760 reproduced. Canonical headers the gateway computed:\n%s",
			gw.lastCanonicalRequest)
		t.Logf("what minio-go signed instead (whitespace run collapsed):\n%s",
			trimAllMinIO(badCD))
	}
}

// TestIssue760_MinIOGatewayAcceptsSameRequest shows the failure is a
// cross-implementation disagreement, not a malformed signature: a gateway
// that trims the minio-go way accepts the very same request. This is why the
// bug is invisible on the MinIO backend (MinIO server ships the same
// signV4TrimAll) and only bites on COS / AWS S3.
func TestIssue760_MinIOGatewayAcceptsSameRequest(t *testing.T) {
	srv, gw := repro760NewServer(t, trimAllMinIO)

	status, cd := repro760Upload(t, gw, srv, "Octo  upload test.pdf")
	t.Logf("double space vs minio-style gateway → PUT %d  CD=%q", status, cd)
	assert.Equal(t, http.StatusOK, status,
		"same request is accepted by a gateway using minio-go's Trimall")
}

// TestIssue760_TrimAllImplementationsDiffer isolates the disagreement itself,
// with no HTTP involved: the two Trimall rules produce different canonical
// values for the very header octo-server signs today.
func TestIssue760_TrimAllImplementationsDiffer(t *testing.T) {
	cd := BuildContentDisposition(sanitizeFilename("Octo  upload test.pdf"))
	minioSide := trimAllMinIO(cd)
	cosSide := trimAllQuotedAware(cd)

	t.Logf("header signed : %q", cd)
	t.Logf("minio-go canon: %q", minioSide)
	t.Logf("COS/AWS canon : %q", cosSide)

	if minioSide == cosSide {
		t.Log("no disagreement: the emitted header is Trimall-stable (GH#760 fixed)")
		return
	}
	assert.Equal(t, cd, cosSide,
		"COS keeps whitespace inside the quoted string, i.e. it canonicalises the header verbatim")
	assert.NotEqual(t, cd, minioSide,
		"minio-go collapses the run inside the quoted string — the source of the signature mismatch")
}

// TestIssue760_CharacterMatrix reproduces the 25-class character matrix from
// the issue against the COS-side rule. Every documented outcome is explained
// by the single whitespace-run mechanism.
func TestIssue760_CharacterMatrix(t *testing.T) {
	srv, gw := repro760NewServer(t, trimAllQuotedAware)

	// `want` records the observed pre-fix outcome for documentation. The
	// assertion uses wantStatusFor so the table stays valid after the fix
	// flips the 403 rows to 200.
	cases := []struct {
		name     string
		filename string
		want     int
		why      string
	}{
		{"single U+0020", "Octo upload test.pdf", http.StatusOK,
			"no whitespace run to collapse"},
		{"two U+0020", "Octo  upload test.pdf", http.StatusForbidden,
			"run inside filename=\"...\" collapsed by signer, kept by gateway"},
		{"three U+0020", "Octo   upload test.pdf", http.StatusForbidden,
			"same mechanism, any run length >= 2"},
		{"two runs of U+0020", "Octo  upload  test.pdf", http.StatusForbidden,
			"same mechanism"},
		{"tab", "Octo\tupload test.pdf", http.StatusOK,
			"sanitizeFilename maps r < 0x20 to '_' before the header is built"},
		{"ideographic space U+3000", "Octo　upload test.pdf", http.StatusOK,
			"non-ASCII branch: '_' in the quoted fallback, %-encoded in filename*"},
		{"no-break space U+00A0", "Octo upload test.pdf", http.StatusOK,
			"non-ASCII branch (note: unicode.IsSpace(U+00A0) is true, so it WOULD collapse if it survived)"},
		{"narrow NBSP U+202F", "Octo upload test.pdf", http.StatusOK,
			"non-ASCII branch"},
		{"zero width space U+200B", "Octo​upload test.pdf", http.StatusOK,
			"non-ASCII branch"},
		{"chinese with single space", "报告 文档.pdf", http.StatusOK,
			"non-ASCII branch, quoted fallback is all '_'"},
		// Wider than the issue's matrix: the non-ASCII branch only replaces
		// runes > 127 with '_', so ASCII spaces survive verbatim in the
		// quoted fallback. A CJK filename with consecutive spaces fails too
		// — the issue's "中文 → 200" row only held because that sample
		// carried a single space.
		{"chinese with double space", "报告  文档.pdf", http.StatusForbidden,
			"non-ASCII branch keeps ASCII spaces: filename=\"__  __.pdf\" still has a run"},
		{"emoji with single space", "photo\U0001F600 x.jpg", http.StatusOK,
			"non-ASCII branch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, cd := repro760Upload(t, gw, srv, tc.filename)
			t.Logf("PUT %d (pre-fix: %d)  CD=%q  (%s)", status, tc.want, cd, tc.why)
			assert.Equal(t, wantStatusFor(cd), status)
		})
	}
}

// TestIssue760_SignedHeaderInvariant states the property that actually has to
// hold for SigV4 agreement, independent of any gateway: a signed header value
// must already be Trimall-stable, i.e. collapsing its whitespace runs must be
// a no-op.
//
// This is the acceptance assertion for the GH#760 fix, and now its permanent
// regression guard: it is stricter than enumerating filenames, because
// anything that puts a whitespace run into a signed header trips it.
func TestIssue760_SignedHeaderInvariant(t *testing.T) {
	filenames := []string{
		"Octo upload test.pdf",
		"Octo  upload test.pdf",
		"Octo   upload  test.pdf",
		"  leading.pdf",
		"trailing  .pdf",
		"report.pdf",
		"报告 文档.pdf",
		"报告  文档.pdf",
		"photo\U0001F600  x.jpg",
	}
	for _, raw := range filenames {
		t.Run(raw, func(t *testing.T) {
			cd := BuildContentDisposition(sanitizeFilename(raw))
			assert.Equal(t, cd, trimAllMinIO(cd),
				"Content-Disposition must be stable under SigV4 Trimall; got %q", cd)
		})
	}
}

// TestIssue760_EchoedContentTypeMatchesSigned covers the second header that
// can carry a client-controlled whitespace run.
//
// The property that matters is NOT "the signed value is collapsed" — minio-go
// runs signV4TrimAll over whatever it is given, so the signature is identical
// either way and asserting on presignPutHeaders' output proves nothing. What
// breaks uploads is the client echoing a value the gateway canonicalizes
// differently, so the assertion is: the value handed to the client must equal
// the value that ends up signed, and both must be Trimall-stable.
//
// getUploadCredentials only overrides the caller's contentType when
// mime.TypeByExtension resolves the extension; the prod image is alpine with
// no /etc/mime.types, so .docx / .xlsx / .zip fall through to the raw query
// value and this is reachable in production.
func TestIssue760_EchoedContentTypeMatchesSigned(t *testing.T) {
	contentTypes := []string{
		"application/octet-stream",
		`application/vnd.openxmlformats-officedocument.wordprocessingml.document; name="a  b.docx"`,
		"application/pdf;  charset=utf-8",
		"text/plain;\t\tcharset=utf-8",
		"  application/json  ",
	}
	for _, raw := range contentTypes {
		t.Run(raw, func(t *testing.T) {
			// What getUploadCredentials puts in resp["contentType"].
			echoed := collapseSignableWhitespace(raw)
			// What ends up in the SigV4 canonical headers for that value.
			signed := trimAllMinIO(presignPutHeaders(echoed, "", 4096).Get("Content-Type"))

			assert.Equal(t, echoed, signed,
				"client echoes %q but the signature covers %q", echoed, signed)
			assert.Equal(t, echoed, trimAllQuotedAware(echoed),
				"echoed Content-Type must also be stable under the COS/AWS rule")
		})
	}
}

// triggers760 reports whether a filename, once put through the production
// handler path, yields a Content-Disposition that SigV4 Trimall would rewrite
// — the exact predicate for the 403. No gateway needed. Post-fix this is
// false for every input; that is what the tables below assert.
func triggers760(rawFilename string) (bool, string) {
	cd := BuildContentDisposition(sanitizeFilename(rawFilename))
	return trimAllMinIO(cd) != cd, cd
}

// naiveContentDisposition reconstructs the pre-fix header exactly as
// BuildContentDisposition emitted it before GH#760 was closed: the quoted
// ASCII fallback keeps whatever whitespace the filename carried.
//
// Keeping it in the test file lets the tables below assert BOTH halves of the
// fix — that the shipped code is now Trimall-stable, and that each "trigger"
// row really was a defect rather than a case that never reproduced. Without
// it, a regression that quietly stopped exercising these names would still
// look green.
func naiveContentDisposition(filename string) string {
	if filename == "" {
		return ""
	}
	encoded := rfc5987Encode(filename)
	var fallback strings.Builder
	for _, r := range filename {
		if r > 127 {
			fallback.WriteRune('_')
		} else {
			fallback.WriteRune(r)
		}
	}
	safe := strings.ReplaceAll(fallback.String(), `\`, `\\`)
	safe = strings.ReplaceAll(safe, `"`, `\"`)
	return "inline; filename=\"" + safe + "\"; filename*=UTF-8''" + encoded
}

func triggersPreFix(rawFilename string) bool {
	cd := naiveContentDisposition(sanitizeFilename(rawFilename))
	return trimAllMinIO(cd) != cd
}

// TestIssue760_TriggerRule pins the trigger down to one rule: the quoted
// ASCII fallback must carry a run of two or more literal spaces. Everything
// else about the name — length, script, punctuation, extension — is
// irrelevant.
//
// `wasTrigger` is the pre-fix outcome. Every row must now be stable under
// Trimall (no 403 possible), and every `wasTrigger` row must still be a case
// the pre-fix construction would have broken on.
func TestIssue760_TriggerRule(t *testing.T) {
	cases := []struct {
		name       string
		wasTrigger bool
		why        string
	}{
		// --- triggers ---
		{"Octo  upload test.pdf", true, "two U+0020"},
		{"Octo   upload test.pdf", true, "three U+0020"},
		{"  leading.pdf", true, "run at the start of the name"},
		{"trailing  .pdf", true, "run before the extension"},
		{"报告  文档.pdf", true, "CJK is irrelevant; the ASCII spaces survive"},
		{"photo\U0001F600  x.jpg", true, "emoji is irrelevant"},
		{"a  b.pdf", true, "shortest possible trigger"},

		// --- does not trigger ---
		{"Octo upload test.pdf", false, "single space"},
		{" leading.pdf", false, "single leading space"},
		{"a\tb.pdf", false, "tab is replaced by '_' in sanitizeFilename"},
		{"a\t\tb.pdf", false, "even a tab run never reaches the header"},
		{"a　　b.pdf", false, "U+3000 replaced by '_' in the quoted fallback"},
		{"a  b.pdf", false, "U+00A0 likewise"},
		{"a  b.pdf", false, "U+202F likewise"},
		{"a​​b.pdf", false, "U+200B likewise"},
		{"报告 文档.pdf", false, "CJK with a single space"},
		{"very-long-name-without-any-spaces-at-all-0123456789.xlsx", false, "length alone is not a trigger"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cd := triggers760(tc.name)
			t.Logf("trigger=%-5v (pre-fix %-5v) cd=%q  (%s)", got, tc.wasTrigger, cd, tc.why)
			assert.False(t, got, "shipped header must be Trimall-stable")
			assert.Equal(t, tc.wasTrigger, triggersPreFix(tc.name),
				"pre-fix classification of this row changed — the table no longer describes the defect")
		})
	}
}

// TestIssue760_ReportedFilenames runs the names actually reported across
// octo-server#760 and octo-web#348 / #1312 through the same predicate.
//
// Only the #760 fixtures — the one reporter who stated space counts
// explicitly — trigger. Every other reported name, as transcribed in the
// issue text, does NOT reproduce. That is expected rather than
// contradictory: rendered Markdown collapses runs of spaces, and several of
// those names are visibly truncated, so the transcriptions cannot preserve
// the trigger.
func TestIssue760_ReportedFilenames(t *testing.T) {
	reported := []struct {
		source     string
		filename   string
		wasTrigger bool
	}{
		{"#760 fixture 02-FAIL", "02-FAIL-Octo  upload test.pdf", true},
		{"#760 fixture 01-PASS", "01-PASS-Octo upload test.pdf", false},
		{"#348 as transcribed (xlsx)", "Lincoln 2026 Q2 Lincoln Z Launch Campaign Jun Autohome.xlsx", false},
		{"#348 as transcribed (pdf)", "Miaozhen Onboarding.pdf", false},
		{"#348 as transcribed (docx)", "中考体育跑鞋推荐 推荐榜单 中考体育跑鞋推荐清单，柔态碳板更适配青少年体测.docx", false},
		{"#348 truncated in report (pptx)", "【结案报告-更新2】2.pptx", false},
		{"#1312 truncated in report (zip)", "0807 设置中心 - 设计.zip", false},
	}

	for _, r := range reported {
		t.Run(r.source, func(t *testing.T) {
			got, cd := triggers760(r.filename)
			t.Logf("trigger=%-5v (pre-fix %-5v) %q\n           cd=%q",
				got, r.wasTrigger, r.filename, cd)
			assert.False(t, got, "shipped header must be Trimall-stable")
			assert.Equal(t, r.wasTrigger, triggersPreFix(r.filename))
		})
	}
}
