package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// projectCodes returns every registered err.server.project.* code.
func projectCodes() []codes.Code {
	var out []codes.Code
	for _, c := range codes.All() {
		if strings.HasPrefix(c.ID, "err.server.project.") {
			out = append(out, c)
		}
	}
	return out
}

// moduleSourceFiles returns every non-test .go file in this package.
//
// Enumerated from disk rather than hand-listed on purpose. The brief warns that a
// guard scoped to api*.go would miss middleware.go — which is the file most likely to
// grow a raw error response, since the SpaceMiddleware it is modelled on uses
// c.AbortWithStatusJSON(gin.H{"msg": ...}) with hardcoded Chinese. A hand-maintained
// list has the same failure one file later: whoever adds project_foo.go also has to
// remember to add it here, and nothing fails if they do not.
func moduleSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		t.Fatal("no source files found; the guard would pass vacuously")
	}
	return files
}

// stripComments removes // comments so a commented-out breadcrumb does not trip a
// guard, and so prose describing a banned pattern is not mistaken for the pattern.
// stripComments removes // comments and then JOINS the physical lines into one string with
// the Go string-concatenation glue (" + ") removed and backticks flattened.
//
// The joining is the point: the guards that scan SQL text used to match line by line, and
// this repo's dominant style splits SQL across continuation lines — so
// `tx.UpdateBySql("UPDATE octo_project SET " + "is_official = 1 ...")` had the column and the
// write marker on different physical lines and evaded both the is_official and the
// member_epoch guards (yujiawei Q9, PR #841 round 1). Joined text closes that class.
func stripComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		out.WriteString(line)
		out.WriteByte(' ')
	}
	joined := out.String()
	for _, glue := range []string{"\" + \"", "\"+\"", "\" +\"", "\"+ \""} {
		joined = strings.ReplaceAll(joined, glue, "")
	}
	joined = strings.ReplaceAll(joined, "`", "")
	return joined
}

func readStripped(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return stripComments(string(data))
}

// TestProjectNoLegacyResponseError pins that no handler OR MIDDLEWARE in this module
// writes a raw error response, bypassing the localized envelope.
func TestProjectNoLegacyResponseError(t *testing.T) {
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
		".ResponseWithStatus(",
		".AbortWithStatusJSON(",
		".AbortWithStatus(",
		"c.Response(\"",
	}
	for _, f := range moduleSourceFiles(t) {
		t.Run(f, func(t *testing.T) {
			cleaned := readStripped(t, f)
			for _, b := range banned {
				if strings.Contains(cleaned, b) {
					t.Errorf("modules/project/%s must use httperr.ResponseErrorL via respondProject* / "+
						"errcode.ErrProject* instead of legacy %s", f, b)
				}
			}
		})
	}
}

// TestMemberEpochOnlyEverIncrements pins the write discipline that IS the
// monotonicity guarantee.
//
// A read-only reconcile scan cannot establish that member_epoch never goes backwards:
// it would have to remember the previous value, and it runs on every pod, so an
// observed "regression" could be two replicas reading either side of one increment.
// So monotonicity holds by construction — every write is `member_epoch =
// member_epoch + 1` — and this test is what keeps that true. reconcile.go's
// best-effort anomaly counter is a diagnostic, not the guarantee.
func TestMemberEpochOnlyEverIncrements(t *testing.T) {
	found := false
	assignment := regexp.MustCompile(`member_epoch\s*=`)
	increment := regexp.MustCompile(`member_epoch\s*=\s*member_epoch\s*\+\s*1`)
	setCall := regexp.MustCompile(`Set\(\s*"member_epoch"`)
	setMap := regexp.MustCompile(`"member_epoch"\s*:`)

	for _, f := range moduleSourceFiles(t) {
		// joined: continuation lines and backticks are flattened, so
		// `SET " + "member_epoch = 0` or a SetMap entry cannot slip between the lines.
		cleaned := readStripped(t, f)
		if setCall.MatchString(cleaned) || setMap.MatchString(cleaned) {
			t.Errorf("modules/project/%s writes member_epoch through a dbr Set()/SetMap; "+
				"only `member_epoch = member_epoch + 1` is allowed, because monotonicity is "+
				"guaranteed by the write shape rather than observed by the reconcile scan", f)
			continue
		}
		for _, m := range assignment.FindAllStringIndex(cleaned, -1) {
			window := cleaned[m[0]:min(m[1]+60, len(cleaned))]
			if !increment.MatchString(window) {
				t.Errorf("modules/project/%s assigns member_epoch to something other than "+
					"member_epoch + 1: %q", f, strings.TrimSpace(window))
			}
			found = true
		}
	}
	if !found {
		t.Error("no `member_epoch = member_epoch + 1` statement found; either the increment " +
			"moved out of this package or the guard stopped matching it")
	}
}

// TestIsOfficialHasNoWriter pins D6 at the source level: the column ships with the
// table but no P0 code path writes it.
//
// Source-level as well as behavioural (see TestIsOfficialStaysZeroThroughCRUD) because
// the behavioural test can only prove the paths it exercises, and a column list derived
// from a struct would write is_official with the value 0 — identical to the default — so
// that regression is invisible to any assertion on the stored value.
//
// Two complementary checks: the real write-side column list must not contain it, and no
// line may pair it with a write marker. Mentioning the column in a SELECT projection or
// in an error string is fine; that is why this does not simply ban the identifier.
func TestIsOfficialHasNoWriter(t *testing.T) {
	for _, col := range projectInsertColumns {
		if col == "is_official" || col == "active_name" || col == "member_epoch" {
			t.Errorf("projectInsertColumns contains %q; is_official has no P0 writer (D6), "+
				"active_name is a generated column (MySQL 3105), and member_epoch may only be "+
				"written as member_epoch + 1", col)
		}
	}

	writeMarker := regexp.MustCompile(`(?i)(insert\s+into|update\s+\S|\bSet\(|\bColumns\(|\bVALUES\b)`)
	for _, f := range moduleSourceFiles(t) {
		cleaned := readStripped(t, f)
		// Whole-file match on joined text: a write naming is_official across continuation
		// lines used to evade the per-line scan.
		for _, m := range regexp.MustCompile(`is_official`).FindAllStringIndex(cleaned, -1) {
			window := cleaned[max(0, m[0]-80):min(m[1]+80, len(cleaned))]
			if writeMarker.MatchString(window) {
				t.Errorf("modules/project/%s pairs is_official with a write marker near %q — "+
					"P0 guarantees the column exists and is never written (D6)",
					f, strings.TrimSpace(window))
			}
		}
	}
}

// TestActiveNameNeverWritten pins D4's implementation note. active_name is a STORED
// generated column and MySQL rejects any INSERT/UPDATE that names it with error 3105,
// so it must appear nowhere except a SELECT projection (and it does not even appear
// there today).
func TestActiveNameNeverWritten(t *testing.T) {
	for _, f := range moduleSourceFiles(t) {
		cleaned := readStripped(t, f)
		if strings.Contains(cleaned, "active_name") {
			t.Errorf("modules/project/%s references active_name; naming a generated column in an "+
				"INSERT/UPDATE is MySQL error 3105, and no read needs it either", f)
		}
	}
}

// TestSpaceMembershipCacheIsNotReimplemented pins the cache-ownership split.
//
// This module must NOT mint its own copy of the `space:member:{spaceID}:{uid}` fact.
// modules/space deletes that key synchronously, inside the request, when it removes
// someone; a second copy under a project: key would have nobody to invalidate it and a
// removed Space member would keep passing the Project Space-gate for a full TTL.
func TestSpaceMembershipCacheIsNotReimplemented(t *testing.T) {
	for _, f := range moduleSourceFiles(t) {
		cleaned := readStripped(t, f)
		if strings.Contains(cleaned, `"space:member:`) {
			t.Errorf("modules/project/%s builds a space:member:* key itself; use "+
				"spacepkg.NewRedisMembershipCache so the Space module's synchronous "+
				"invalidation applies to it", f)
		}
	}
	// And the project's own cache stays inside the project: namespace, which collides
	// with neither space:member:* nor ratelimit:*.
	if got := projectMemberCacheKey("p1", "u1"); got != "project:member:p1:u1" {
		t.Fatalf("project member cache key = %q, want the project: namespace", got)
	}
}

// TestAuthChainOrder asserts on the ROUTE CHAIN, not on observed 429s.
//
// SharedUIDRateLimiter reads the uid that AuthMiddleware puts in the context, and it
// fails OPEN when there is none. Mounted before AuthMiddleware the route therefore
// looks rate-limited and is not — and a "hammer it until it 429s" test would pass
// either way, because the global per-IP limiter in main.go would eventually answer.
// So the ordering is checked where it is declared.
func TestAuthChainOrder(t *testing.T) {
	src := readStripped(t, "api.go")

	groups := regexp.MustCompile(`r\.Group\(\s*"([^"]+)"`).FindAllStringSubmatch(src, -1)
	if len(groups) == 0 {
		t.Fatal("no r.Group(...) found in api.go; the guard would pass vacuously")
	}
	chain := regexp.MustCompile(
		`r\.Group\(\s*"[^"]+",\s*p\.ctx\.AuthMiddleware\(r\),\s*appwkhttp\.SharedUIDRateLimiter\(r, p\.ctx\),`)
	matches := chain.FindAllString(src, -1)
	if len(matches) != len(groups) {
		t.Fatalf("api.go declares %d route groups but only %d mount "+
			"AuthMiddleware immediately followed by SharedUIDRateLimiter; every authenticated "+
			"group must, and P0 mounts no unauthenticated group", len(groups), len(matches))
	}
}

// ---------- envelope assertions ----------

type projectErrEnvelope struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

func decodeProjectEnvelope(t *testing.T, body []byte) projectErrEnvelope {
	t.Helper()
	var env projectErrEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, body)
	}
	return env
}

func assertProjectErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	env := decodeProjectEnvelope(t, w.Body.Bytes())
	if env.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q\nbody: %s", env.Error.Code, wantCode, w.Body.String())
	}
}

// projectHelperHarness mounts one probe route with the i18n renderer wired, so the
// helper assertions need no DB or auth.
func projectHelperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondProjectHelpers(t *testing.T) {
	cases := []struct {
		name          string
		probe         func(c *wkhttp.Context)
		wantCodeID    string
		wantSemStatus int
		wantDetails   map[string]any
	}{
		{
			name:          "request invalid carries the field",
			probe:         func(c *wkhttp.Context) { respondProjectRequestInvalid(c, "uids") },
			wantCodeID:    "err.server.project.request_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantDetails:   map[string]any{"field": "uids"},
		},
		{
			name:          "request invalid omits an empty field",
			probe:         func(c *wkhttp.Context) { respondProjectRequestInvalid(c, "") },
			wantCodeID:    "err.server.project.request_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantDetails:   map[string]any{},
		},
		{
			name:          "name invalid surfaces the cap",
			probe:         func(c *wkhttp.Context) { respondProjectNameInvalid(c) },
			wantCodeID:    "err.server.project.name_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantDetails:   map[string]any{"field": "name", "max_chars": float64(maxNameChars)},
		},
		{
			name:          "batch too large surfaces the cap",
			probe:         func(c *wkhttp.Context) { respondProjectBatchTooLarge(c, 200) },
			wantCodeID:    "err.server.project.batch_too_large",
			wantSemStatus: http.StatusBadRequest,
			wantDetails:   map[string]any{"max": float64(200)},
		},
		{
			name:          "quota surfaces the configured max",
			probe:         func(c *wkhttp.Context) { respondProjectQuota(c, errcode.ErrProjectQuotaPerSpace, 500) },
			wantCodeID:    "err.server.project.quota_per_space",
			wantSemStatus: http.StatusForbidden,
			wantDetails:   map[string]any{"max": float64(500)},
		},
		{
			// The anti-enumeration answer must carry NO details, or the three reasons
			// that produce it stop being indistinguishable.
			name:          "not found carries no details",
			probe:         func(c *wkhttp.Context) { respondProjectNotFound(c) },
			wantCodeID:    "err.server.project.not_found",
			wantSemStatus: http.StatusNotFound,
			wantDetails:   map[string]any{},
		},
		{
			name:          "internal query failure hides its message",
			probe:         func(c *wkhttp.Context) { respondQueryFailed(c) },
			wantCodeID:    "err.server.project.query_failed",
			wantSemStatus: http.StatusInternalServerError,
			wantDetails:   map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := projectHelperHarness(tc.probe)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))

			// D5: ResponseErrorL pins the WIRE status to 400 for legacy compatibility;
			// the real status travels in error.http_status.
			if w.Code != http.StatusBadRequest {
				t.Fatalf("wire status = %d, want 400 (ResponseErrorL pins it)", w.Code)
			}
			env := decodeProjectEnvelope(t, w.Body.Bytes())
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
			}
			if len(env.Error.Details) != len(tc.wantDetails) {
				t.Fatalf("details = %#v, want %#v", env.Error.Details, tc.wantDetails)
			}
			for k, v := range tc.wantDetails {
				if env.Error.Details[k] != v {
					t.Fatalf("details[%q] = %#v, want %#v", k, env.Error.Details[k], v)
				}
			}
		})
	}
}

// TestInternalCodesAreFiveHundredAndOnlyThose pins the 5xx <=> Internal invariant for
// this module's codes: the renderer only hides a message when Internal is set, and a
// 4xx marked Internal would silently stop telling clients what they got wrong.
func TestInternalCodesAreFiveHundredAndOnlyThose(t *testing.T) {
	internal := map[string]bool{
		"err.server.project.query_failed": true,
		"err.server.project.store_failed": true,
	}
	for _, c := range projectCodes() {
		isFiveXX := c.HTTPStatus >= 500
		if isFiveXX != c.Internal {
			t.Errorf("%s: HTTPStatus=%d Internal=%v — 5xx and Internal must agree",
				c.ID, c.HTTPStatus, c.Internal)
		}
		if c.Internal != internal[c.ID] {
			t.Errorf("%s: Internal=%v but the expected internal set says %v",
				c.ID, c.Internal, internal[c.ID])
		}
	}
}
