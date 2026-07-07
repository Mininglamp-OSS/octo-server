package messages_search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// stubAnalyzer feeds buildKeywordClause a canned token list (or error) so the
// keyword-DSL shape can be asserted without an OpenSearch cluster. Tests pass
// either tokens (happy path) or err (fallback path); both can be set on the
// same stub but tokens are ignored when err is non-nil — same as the real
// IndexAnalyze().Do() contract.
type stubAnalyzer struct {
	tokens []string
	err    error
}

func (s stubAnalyzer) Analyze(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.tokens, nil
}

// countingAnalyzer wraps a stubAnalyzer with a call counter. Used by the
// flag-off tests to verify the `_analyze` roundtrip is actually skipped
// (a query-shape assertion alone can't distinguish "analyze ran, fell back
// to raw" from "analyze never ran").
type countingAnalyzer struct {
	inner stubAnalyzer
	calls int
}

func (c *countingAnalyzer) Analyze(ctx context.Context, text string) ([]string, error) {
	c.calls++
	return c.inner.Analyze(ctx, text)
}

// keywordClauseBody marshals a buildKeywordClause result to JSON so substring
// assertions stay legible. The wrapper returned by olivere matches the OS
// wire shape ({ "multi_match": { ... } }), which is what we want to pin.
func keywordClauseBody(t *testing.T, q interface {
	Source() (any, error)
}) string {
	t.Helper()
	src, err := q.Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestBuildKeywordClause_PureStopword_SingleToken(t *testing.T) {
	// "的" alone — user wants literal matches of the function word.
	// Expected: original keyword, cross_fields, NO MSM.
	a := stubAnalyzer{tokens: []string{"的"}}
	q, err := buildKeywordClause(context.Background(), a, "的", "payload.text.content")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"的"`) {
		t.Errorf("pure stopword path must use the original keyword:\n%s", body)
	}
	if !strings.Contains(body, `"type":"cross_fields"`) {
		t.Errorf("missing cross_fields type:\n%s", body)
	}
	if strings.Contains(body, "minimum_should_match") {
		t.Errorf("pure stopword path must NOT set MSM (literal match semantic):\n%s", body)
	}
}

func TestBuildKeywordClause_PureStopword_MultiCharToken(t *testing.T) {
	// IK smart segments "的话" as a single token; it's in the stopword set
	// and after filtering nothing remains → pure-stopword path.
	a := stubAnalyzer{tokens: []string{"的话"}}
	q, err := buildKeywordClause(context.Background(), a, "的话", "payload.text.content")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"的话"`) {
		t.Errorf("expected original keyword '的话':\n%s", body)
	}
	if strings.Contains(body, "minimum_should_match") {
		t.Errorf("pure stopword must skip MSM:\n%s", body)
	}
}

func TestBuildKeywordClause_PureStopword_AllStrippedAway(t *testing.T) {
	// "的了" → ["的","了"] → after filter both gone. Must fall back to the
	// original (unsplit) keyword so the user still gets literal hits.
	a := stubAnalyzer{tokens: []string{"的", "了"}}
	q, err := buildKeywordClause(context.Background(), a, "的了", "payload.text.content")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"的了"`) {
		t.Errorf("post-strip empty must use the ORIGINAL keyword, not the join:\n%s", body)
	}
	if strings.Contains(body, "minimum_should_match") {
		t.Errorf("post-strip empty must NOT set MSM:\n%s", body)
	}
}

func TestBuildKeywordClause_ContentWithStopword_OrTrapFix(t *testing.T) {
	// The reproducer from the spec: 5-token query with a single stopword. The
	// "的" must be dropped from the issued query so MSM 75% applies to the 4
	// content words.
	a := stubAnalyzer{tokens: []string{"按时", "缴纳", "时间", "的", "女性"}}
	q, err := buildKeywordClause(context.Background(), a, "按时缴纳时间的女性",
		"payload.text.content^3", "payload.mergeForward.msgs.searchText",
	)
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"按时 缴纳 时间 女性"`) {
		t.Errorf("filtered keyword must be the space-joined content tokens, no '的':\n%s", body)
	}
	if !strings.Contains(body, `"type":"cross_fields"`) {
		t.Errorf("missing cross_fields type:\n%s", body)
	}
	if !strings.Contains(body, `"minimum_should_match":"75%"`) {
		t.Errorf("missing MSM 75%%:\n%s", body)
	}
}

func TestBuildKeywordClause_ShortMixed(t *testing.T) {
	// "在公司" → ["在","公司"]. "在" is a stopword → MSM 75% on the lone
	// content word "公司" trivially satisfies, the high-frequency "在"
	// no longer drags noise into the result set.
	a := stubAnalyzer{tokens: []string{"在", "公司"}}
	q, err := buildKeywordClause(context.Background(), a, "在公司", "payload.text.content")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"公司"`) {
		t.Errorf("expected stripped query '公司':\n%s", body)
	}
	if !strings.Contains(body, `"minimum_should_match":"75%"`) {
		t.Errorf("expected MSM 75%%:\n%s", body)
	}
}

func TestBuildKeywordClause_AllContentWords(t *testing.T) {
	// "季度报告" → ["季度","报告"]; both content words preserved → MSM 75%
	// on the joined tokens.
	a := stubAnalyzer{tokens: []string{"季度", "报告"}}
	q, err := buildKeywordClause(context.Background(), a, "季度报告", "payload.file.name^2", "payload.file.caption")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"季度 报告"`) {
		t.Errorf("expected joined query '季度 报告':\n%s", body)
	}
	if !strings.Contains(body, `"type":"cross_fields"`) {
		t.Errorf("missing cross_fields type:\n%s", body)
	}
	if !strings.Contains(body, `"minimum_should_match":"75%"`) {
		t.Errorf("missing MSM 75%%:\n%s", body)
	}
	if !strings.Contains(body, `"payload.file.name^2"`) || !strings.Contains(body, `"payload.file.caption"`) {
		t.Errorf("fields not propagated:\n%s", body)
	}
}

func TestBuildKeywordClause_AnalyzeError_FallbackDegrades(t *testing.T) {
	// Spec §4.4: when `_analyze` is unreachable, degrade to "raw keyword +
	// cross_fields + MSM 75%" and surface the error so the handler can log.
	a := stubAnalyzer{err: errors.New("ik unavailable")}
	q, err := buildKeywordClause(context.Background(), a, "按时缴纳时间的女性", "payload.text.content")
	if err == nil {
		t.Fatalf("expected error to propagate so caller can warn-log")
	}
	body := keywordClauseBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"按时缴纳时间的女性"`) {
		t.Errorf("fallback must use the original keyword unmodified:\n%s", body)
	}
	if !strings.Contains(body, `"type":"cross_fields"`) {
		t.Errorf("fallback must still apply cross_fields:\n%s", body)
	}
	if !strings.Contains(body, `"minimum_should_match":"75%"`) {
		t.Errorf("fallback must still apply MSM 75%% to dampen OR-trap:\n%s", body)
	}
}

// queryBody marshals a builder result for substring assertions. Same idea as
// keywordClauseBody, but accepts the no-error builder return type.
func queryBody(t *testing.T, q interface {
	Source() (any, error)
}) string {
	t.Helper()
	src, err := q.Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestAnalyzeKeyword_PureStopword pins the second branch of the analyze
// contract: when every token falls into the stopword set, return the
// ORIGINAL keyword + useMSM=false so the builder downstream emits a raw
// no-MSM clause (literal-stopword-search semantic).
func TestAnalyzeKeyword_PureStopword(t *testing.T) {
	a := stubAnalyzer{tokens: []string{"的", "了"}}
	eff, useMSM, err := AnalyzeKeyword(context.Background(), a, "的了")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	if eff != "的了" {
		t.Errorf("pure-stopword branch must return the ORIGINAL keyword unsplit, got %q", eff)
	}
	if useMSM {
		t.Errorf("pure-stopword branch must NOT request MSM (literal-search semantic)")
	}
}

// TestAnalyzeKeyword_ContentWords pins the third branch: surviving content
// tokens are space-joined and the builder must apply MSM 75%.
func TestAnalyzeKeyword_ContentWords(t *testing.T) {
	a := stubAnalyzer{tokens: []string{"按时", "缴纳", "时间", "的", "女性"}}
	eff, useMSM, err := AnalyzeKeyword(context.Background(), a, "按时缴纳时间的女性")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	if eff != "按时 缴纳 时间 女性" {
		t.Errorf("expected stopword-stripped join, got %q", eff)
	}
	if !useMSM {
		t.Errorf("content-word branch must request MSM 75%%")
	}
}

// TestAnalyzeKeyword_FallbackOnError pins §4.4 degradation: when `_analyze`
// is unreachable the caller receives (rawKeyword, useMSM=true, err) so the
// downstream builder emits raw + cross_fields + MSM 75% and the handler can
// warn-log the surfaced error.
func TestAnalyzeKeyword_FallbackOnError(t *testing.T) {
	a := stubAnalyzer{err: errors.New("ik unavailable")}
	eff, useMSM, err := AnalyzeKeyword(context.Background(), a, "按时缴纳时间的女性")
	if err == nil {
		t.Fatalf("expected analyze error to propagate")
	}
	if eff != "按时缴纳时间的女性" {
		t.Errorf("fallback must return the original keyword unmodified, got %q", eff)
	}
	if !useMSM {
		t.Errorf("fallback must keep MSM 75%% (the §4.4 dampener)")
	}
}

// TestAnalyzeKeyword_PostFilterEmpty: even when the post-filter content
// slice is empty (every token stripped), the returned effectiveKeyword is
// the ORIGINAL string — not the joined empty slice (which would be "") —
// so the multi_match doesn't ship with an empty query body.
func TestAnalyzeKeyword_PostFilterEmpty(t *testing.T) {
	a := stubAnalyzer{tokens: []string{}}
	eff, useMSM, err := AnalyzeKeyword(context.Background(), a, "literal")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	if eff != "literal" {
		t.Errorf("empty-token branch must fall back to the original keyword, got %q", eff)
	}
	if useMSM {
		t.Errorf("empty-token branch must NOT request MSM")
	}
}

// TestBuildKeywordClauseFromAnalyzed_OneAnalyzeManyClauses asserts the
// followup #1 invariant: a caller can build any number of multi_match
// clauses (different field sets) from a single AnalyzeKeyword result
// without triggering a second analyze call.
func TestBuildKeywordClauseFromAnalyzed_OneAnalyzeManyClauses(t *testing.T) {
	a := &countingAnalyzer{inner: stubAnalyzer{tokens: []string{"季度", "报告"}}}
	eff, useMSM, err := AnalyzeKeyword(context.Background(), a, "季度报告")
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	text := buildKeywordClauseFromAnalyzed(eff, useMSM, "payload.text.content^3", "payload.mergeForward.msgs.searchText")
	file := buildKeywordClauseFromAnalyzed(eff, useMSM, "payload.file.name^2", "payload.file.caption")
	if a.calls != 1 {
		t.Fatalf("expected exactly one _analyze call for two clauses, got %d", a.calls)
	}
	textBody := queryBody(t, text.(interface {
		Source() (any, error)
	}))
	fileBody := queryBody(t, file.(interface {
		Source() (any, error)
	}))
	for _, want := range []string{`"query":"季度 报告"`, `"type":"cross_fields"`, `"minimum_should_match":"75%"`} {
		if !strings.Contains(textBody, want) {
			t.Errorf("text clause missing %q:\n%s", want, textBody)
		}
		if !strings.Contains(fileBody, want) {
			t.Errorf("file clause missing %q:\n%s", want, fileBody)
		}
	}
	if !strings.Contains(textBody, `"payload.text.content^3"`) {
		t.Errorf("text clause missing text field set:\n%s", textBody)
	}
	if !strings.Contains(fileBody, `"payload.file.name^2"`) {
		t.Errorf("file clause missing file field set:\n%s", fileBody)
	}
}

// TestBuildKeywordClauseGated_FlagOffSkipsAnalyze pins the followup #2 kill
// switch: when stopwordStripEnabled is false the gated builder must (a)
// never call the analyzer (no `_analyze` roundtrip RT) and (b) emit the
// §4.4 degraded shape — raw keyword + cross_fields + MSM 75%.
func TestBuildKeywordClauseGated_FlagOffSkipsAnalyze(t *testing.T) {
	a := &countingAnalyzer{inner: stubAnalyzer{tokens: []string{"按时", "缴纳", "时间", "的", "女性"}}}
	q, err := buildKeywordClauseGated(context.Background(), a, false, "按时缴纳时间的女性",
		"payload.text.content^3", "payload.mergeForward.msgs.searchText",
	)
	if err != nil {
		t.Fatalf("flag-off path must never surface an analyze error: %v", err)
	}
	if a.calls != 0 {
		t.Fatalf("flag-off path must NOT trigger _analyze, got %d calls", a.calls)
	}
	body := queryBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"按时缴纳时间的女性"`) {
		t.Errorf("flag-off must use the RAW keyword unmodified (no stopword strip):\n%s", body)
	}
	if !strings.Contains(body, `"type":"cross_fields"`) {
		t.Errorf("flag-off must keep cross_fields:\n%s", body)
	}
	if !strings.Contains(body, `"minimum_should_match":"75%"`) {
		t.Errorf("flag-off must keep MSM 75%% (§4.4 dampener):\n%s", body)
	}
}

// TestBuildKeywordClauseGated_FlagOnPreservesStripBehavior is the
// counter-test: flag=true keeps the existing buildKeywordClause path
// (analyze runs, stopwords drop, MSM 75% applies to content tokens).
func TestBuildKeywordClauseGated_FlagOnPreservesStripBehavior(t *testing.T) {
	a := &countingAnalyzer{inner: stubAnalyzer{tokens: []string{"按时", "缴纳", "时间", "的", "女性"}}}
	q, err := buildKeywordClauseGated(context.Background(), a, true, "按时缴纳时间的女性",
		"payload.text.content^3",
	)
	if err != nil {
		t.Fatalf("unexpected analyze error: %v", err)
	}
	if a.calls != 1 {
		t.Fatalf("flag-on path must invoke _analyze exactly once, got %d", a.calls)
	}
	body := queryBody(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"query":"按时 缴纳 时间 女性"`) {
		t.Errorf("flag-on must strip stopwords and join with spaces:\n%s", body)
	}
}

// TestBuildSearchAllDSL_OneAnalyzePerKeyword is the followup #1 acceptance
// test at the DSL level: _search_all has two Should branches against the
// same keyword, and after the refactor must share one _analyze call.
func TestBuildSearchAllDSL_OneAnalyzePerKeyword(t *testing.T) {
	a := &countingAnalyzer{inner: stubAnalyzer{tokens: []string{"季度", "报告"}}}
	req := SearchAllReq{ChannelType: channelTypeGroup, ChannelID: "g", Keyword: "季度报告"}
	if _, err := buildSearchAllDSL(context.Background(), a, true, req, "g", ""); err != nil {
		t.Fatalf("unexpected DSL error: %v", err)
	}
	if a.calls != 1 {
		t.Fatalf("_search_all must reuse one _analyze across its two Should branches, got %d calls", a.calls)
	}
}
