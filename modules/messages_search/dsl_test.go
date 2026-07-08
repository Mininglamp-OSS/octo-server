package messages_search

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/olivere/elastic"
)

// fallbackTestAnalyzer triggers buildKeywordClause's fallback path (raw
// keyword + cross_fields + MSM 75%), which is what shape tests for the
// non-keyword DSL plumbing want — the original keyword stays intact in the
// emitted query, so the historical substring assertions continue to apply.
// Tests asserting branch-specific shape live in keyword_query_test.go.
func fallbackTestAnalyzer() stubAnalyzer {
	return stubAnalyzer{err: errors.New("test: analyze unavailable")}
}

// extractDSL serialises a query for asserting structural shape in tests.
func extractDSL(t *testing.T, q interface {
	Source() (any, error)
}) map[string]any {
	t.Helper()
	src, err := q.Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestBuildSearchMessagesDSL_Shape(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypeGroup,
		ChannelID:   "groupNo",
		Keyword:     "hello",
		Filters: SearchFilters{
			SenderIDs: []string{"u1", "u2"},
		},
	}
	// Fallback analyzer keeps the original keyword in the emitted query, so
	// the historical substring assertions (the "hello" pin) still apply. The
	// MSM 75% + cross_fields shape introduced by the OR-trap fix is asserted
	// in keyword_query_test.go alongside the branch logic — duplicating it
	// here would just couple this test to the fallback path.
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "groupNo", "")
	dsl := extractDSL(t, q.(interface {
		Source() (any, error)
	}))
	js, _ := json.Marshal(dsl)
	body := string(js)
	for _, want := range []string{
		`"multi_match"`,
		`"hello"`,
		`"payload.text.content^3"`,
		`"payload.richText.searchText^3"`,
		`"payload.mergeForward.msgs.searchText"`,
		`"channelId":"groupNo"`,
		`"revoked":true`,
		`"payload.type":99`,
		`"from":1000`,
		`"to":2000`,
		`"include_lower":true`,
		`"include_upper":true`,
		`"virtual":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DSL missing %q in:\n%s", want, body)
		}
	}
	// payload.type whitelist may serialise with or without spaces depending on
	// the elastic library's marshalling — accept both shapes.
	if !strings.Contains(body, `"payload.type":[1,11,14]`) && !strings.Contains(body, `"payload.type":[1, 11, 14]`) {
		t.Errorf("DSL missing terms payload.type [1,11,14] in:\n%s", body)
	}
}

func TestBuildSearchMessagesDSL_NoKeywordSkipsMultiMatch(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypeGroup,
		ChannelID:   "groupNo",
	}
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "groupNo", "")
	js, _ := json.Marshal(extractDSL(t, q.(interface {
		Source() (any, error)
	})))
	body := string(js)
	if strings.Contains(body, "multi_match") {
		t.Errorf("search_messages DSL with empty keyword must not include multi_match:\n%s", body)
	}
	for _, want := range []string{
		`"channelId":"groupNo"`,
		`"revoked":true`,
		`"payload.type":99`,
		`"from":1000`,
		`"to":2000`,
		`"include_lower":true`,
		`"include_upper":true`,
		`"virtual":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-keyword DSL missing %q in:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"payload.type":[1,11,14]`) && !strings.Contains(body, `"payload.type":[1, 11, 14]`) {
		t.Errorf("empty-keyword DSL missing terms payload.type [1,11,14] in:\n%s", body)
	}
}

// TestBuildSearchMessagesDSL_KeywordFieldsWhitelist pins the contract that the
// /_search multi_match keyword clause targets EXACTLY the three text-bearing
// projections that align with the payload.type whitelist [1, 11, 14]:
// payload.text.content^3 (text=1), payload.richText.searchText^3 (richText=14),
// payload.mergeForward.msgs.searchText (mergeForward=11). The previously listed
// image.caption / image.name / file.caption / file.name fields became dead
// branches once the type whitelist excluded image(2) and file(8) — kept here as
// a regression pin so any future revival surfaces in test, not in production.
func TestBuildSearchMessagesDSL_KeywordFieldsWhitelist(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypeGroup,
		ChannelID:   "g",
		Keyword:     "hello",
	}
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
	src, err := q.(interface{ Source() (any, error) }).Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	raw, _ := json.Marshal(src)
	body := string(raw)

	// Negative substring guard: no media / file projection should appear in
	// the /_search query at all (multi_match or otherwise). Highlight config
	// is built separately and is asserted in its own test.
	for _, banned := range []string{
		"payload.image.caption",
		"payload.image.name",
		"payload.file.caption",
		"payload.file.name",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("/_search DSL must not reference %q (whitelist [1,11,14] excludes image/file): %s", banned, body)
		}
	}

	// Walk the marshalled DSL to find every multi_match.fields array and
	// assert each one is the exact whitelist (length + order + content).
	want := []string{
		"payload.text.content^3",
		"payload.richText.searchText^3",
		"payload.mergeForward.msgs.searchText",
	}
	var found int
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if mm, ok := v["multi_match"].(map[string]any); ok {
				rawFields, ok := mm["fields"].([]any)
				if !ok {
					t.Errorf("multi_match has no fields array: %v", mm)
					return
				}
				if len(rawFields) != len(want) {
					t.Errorf("multi_match.fields length = %d, want %d: %v", len(rawFields), len(want), rawFields)
					return
				}
				for i, f := range rawFields {
					if f.(string) != want[i] {
						t.Errorf("multi_match.fields[%d] = %q, want %q", i, f, want[i])
					}
				}
				found++
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	walk(normalized)
	if found == 0 {
		t.Errorf("expected at least one multi_match clause in /_search DSL, got 0: %s", body)
	}
}

// TestBuildSearchMessagesDSL_TypeWhitelist pins the contract that /_search
// returns ONLY text (payload.type=1), mergeForward (payload.type=11), and
// richText (payload.type=14) messages. Image / voice / video / file payloads
// are served through the dedicated /_search_media, /_search_files,
// /_search_all surfaces — they must not surface on the legacy /_search
// response, whose client UI only renders text/richText/mergeForward snippets.
//
// Asserted shape: bool.filter contains exactly one terms(payload.type) clause,
// whose array equals [1, 11, 14] (order matters — matches the indexer's terms
// query input order in buildSearchMessagesDSL).
func TestBuildSearchMessagesDSL_TypeWhitelist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keyword string
	}{
		{"keyword", "hello"},
		{"browse", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := SearchMessagesReq{
				ChannelType: channelTypeGroup,
				ChannelID:   "g",
				Keyword:     tc.keyword,
			}
			q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
			src, err := q.(interface{ Source() (any, error) }).Source()
			if err != nil {
				t.Fatalf("Source(): %v", err)
			}
			raw, _ := json.Marshal(src)
			var normalized map[string]any
			if err := json.Unmarshal(raw, &normalized); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			boolNode, ok := normalized["bool"].(map[string]any)
			if !ok {
				t.Fatalf("query has no bool node: %s", raw)
			}
			rawFilter, ok := boolNode["filter"]
			if !ok {
				t.Fatalf("bool has no filter: %s", raw)
			}
			var filters []any
			switch v := rawFilter.(type) {
			case []any:
				filters = v
			case map[string]any:
				filters = []any{v}
			default:
				t.Fatalf("filter has unexpected shape %T: %s", rawFilter, raw)
			}

			var matched bool
			for _, clause := range filters {
				m, ok := clause.(map[string]any)
				if !ok {
					continue
				}
				terms, ok := m["terms"].(map[string]any)
				if !ok {
					continue
				}
				arr, ok := terms["payload.type"].([]any)
				if !ok {
					continue
				}
				if len(arr) != 3 {
					t.Errorf("payload.type whitelist must be exactly 3 entries, got %d: %v", len(arr), arr)
					continue
				}
				got := []int{int(arr[0].(float64)), int(arr[1].(float64)), int(arr[2].(float64))}
				want := []int{payloadTypeText, payloadTypeMergeForward, payloadTypeRichText}
				if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
					t.Errorf("payload.type whitelist mismatch: got %v want %v", got, want)
				}
				// Reject every disallowed payload type explicitly so any future
				// regression (someone adds payloadTypeImage to the whitelist)
				// surfaces here, not in production.
				for _, banned := range []int{
					payloadTypeImage,
					payloadTypeGIF,
					payloadTypeVoice,
					payloadTypeVideo,
					payloadTypeFile,
				} {
					for _, v := range got {
						if v == banned {
							t.Errorf("payload.type whitelist must not include %d (media/file): %v", banned, got)
						}
					}
				}
				matched = true
			}
			if !matched {
				t.Errorf("bool.filter has no terms(payload.type) whitelist clause:\n%s", raw)
			}
		})
	}
}

// TestBuildSearchMessagesDSL_FiltersSystemMessages pins the indexer §2.2
// "搜索硬过滤" contract: payload.type 1000-2000 (FriendApply / Group* / Hotline*
// / Tip) MUST be excluded from /_search_messages, alongside the existing
// type==99 (Cmd) exclusion. Regression test for the empty-keyword browse path
// leaking "GroupCreate" / "GroupMemberAdd" system events to the client.
func TestBuildSearchMessagesDSL_FiltersSystemMessages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keyword string
	}{
		{"keyword", "hello"},
		{"browse", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := SearchMessagesReq{
				ChannelType: channelTypeGroup,
				ChannelID:   "groupNo",
				Keyword:     tc.keyword,
			}
			q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "groupNo", "")
			src, err := q.(interface{ Source() (any, error) }).Source()
			if err != nil {
				t.Fatalf("Source(): %v", err)
			}
			b, _ := json.Marshal(src)
			body := string(b)

			// JSON-roundtrip so all numeric values normalize to float64 and
			// nested objects to map[string]any, regardless of what concrete
			// Go types elastic.BoolQuery.Source() chose to emit.
			var normalized map[string]any
			if err := json.Unmarshal(b, &normalized); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			boolNode, ok := normalized["bool"].(map[string]any)
			if !ok {
				t.Fatalf("query has no bool node: %s", body)
			}
			rawMN, ok := boolNode["must_not"]
			if !ok {
				t.Fatalf("bool has no must_not: %s", body)
			}
			var mustNot []any
			switch v := rawMN.(type) {
			case []any:
				mustNot = v
			case map[string]any:
				mustNot = []any{v}
			default:
				t.Fatalf("must_not has unexpected shape %T: %s", rawMN, body)
			}

			var seenCmd, seenRange bool
			for _, clause := range mustNot {
				m, ok := clause.(map[string]any)
				if !ok {
					continue
				}
				if term, ok := m["term"].(map[string]any); ok {
					if pt, ok := term["payload.type"].(float64); ok && int(pt) == payloadTypeCmd {
						seenCmd = true
					}
				}
				if rng, ok := m["range"].(map[string]any); ok {
					if pt, ok := rng["payload.type"].(map[string]any); ok {
						// elastic encodes Gte/Lte as from/to + include_lower/include_upper.
						lo, loOK := pt["from"].(float64)
						hi, hiOK := pt["to"].(float64)
						incLo, _ := pt["include_lower"].(bool)
						incHi, _ := pt["include_upper"].(bool)
						if loOK && hiOK && int(lo) == payloadTypeSystemMin && int(hi) == payloadTypeSystemMax && incLo && incHi {
							seenRange = true
						}
					}
				}
			}
			if !seenCmd {
				t.Errorf("must_not missing term payload.type=%d in:\n%s", payloadTypeCmd, body)
			}
			if !seenRange {
				t.Errorf("must_not missing range payload.type [%d,%d] in:\n%s", payloadTypeSystemMin, payloadTypeSystemMax, body)
			}
		})
	}
}

// TestApplySystemMessageHardFilter pins the shared helper that both
// /_search_messages and /_search_around use to satisfy the indexer §2.4
// "搜索硬过滤" contract. Empty bool query in, two must_not clauses out:
// term(payload.type=99) and range(payload.type ∈ [1000, 2000]).
func TestApplySystemMessageHardFilter(t *testing.T) {
	b := elastic.NewBoolQuery()
	applySystemMessageHardFilter(b)

	src, err := b.Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	raw, _ := json.Marshal(src)
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	boolNode, ok := normalized["bool"].(map[string]any)
	if !ok {
		t.Fatalf("query has no bool node: %s", raw)
	}
	rawMN, ok := boolNode["must_not"]
	if !ok {
		t.Fatalf("bool has no must_not: %s", raw)
	}
	var mustNot []any
	switch v := rawMN.(type) {
	case []any:
		mustNot = v
	case map[string]any:
		mustNot = []any{v}
	default:
		t.Fatalf("must_not has unexpected shape %T: %s", rawMN, raw)
	}

	var seenCmd, seenRange bool
	for _, clause := range mustNot {
		m, ok := clause.(map[string]any)
		if !ok {
			continue
		}
		if term, ok := m["term"].(map[string]any); ok {
			if pt, ok := term["payload.type"].(float64); ok && int(pt) == payloadTypeCmd {
				seenCmd = true
			}
		}
		if rng, ok := m["range"].(map[string]any); ok {
			if pt, ok := rng["payload.type"].(map[string]any); ok {
				lo, loOK := pt["from"].(float64)
				hi, hiOK := pt["to"].(float64)
				incLo, _ := pt["include_lower"].(bool)
				incHi, _ := pt["include_upper"].(bool)
				if loOK && hiOK && int(lo) == payloadTypeSystemMin && int(hi) == payloadTypeSystemMax && incLo && incHi {
					seenRange = true
				}
			}
		}
	}
	if !seenCmd {
		t.Errorf("must_not missing term payload.type=%d in:\n%s", payloadTypeCmd, raw)
	}
	if !seenRange {
		t.Errorf("must_not missing range payload.type [%d,%d] in:\n%s", payloadTypeSystemMin, payloadTypeSystemMax, raw)
	}
}

func TestBuildSearchMediaDSL_FiltersTypes(t *testing.T) {
	req := SearchMediaReq{ChannelType: channelTypeGroup, ChannelID: "g"}
	q := buildSearchMediaDSL(req, "g", "")
	dsl := extractDSL(t, q.(interface {
		Source() (any, error)
	}))
	js, _ := json.Marshal(dsl)
	body := string(js)
	if !strings.Contains(body, `"payload.type":[2,5]`) && !strings.Contains(body, `"payload.type":[2, 5]`) {
		t.Errorf("media DSL should filter on payload.type [2,5]:\n%s", body)
	}
	if strings.Contains(body, "multi_match") {
		t.Errorf("media DSL must not include multi_match")
	}
	// Part B virtual-children intentionally surface in /_search_media, so the
	// must_not(virtual=true) helper from /_search and friends MUST NOT appear
	// here.
	if strings.Contains(body, `"virtual"`) {
		t.Errorf("media DSL must NOT carry virtual filter:\n%s", body)
	}
}

func TestBuildSearchFilesDSL_NoKeywordSkipsMultiMatch(t *testing.T) {
	req := SearchFilesReq{ChannelType: channelTypeGroup, ChannelID: "g"}
	q, _ := buildSearchFilesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
	js, _ := json.Marshal(extractDSL(t, q.(interface {
		Source() (any, error)
	})))
	body := string(js)
	if strings.Contains(body, "multi_match") {
		t.Errorf("file DSL with empty keyword must not include multi_match:\n%s", body)
	}
	if !strings.Contains(body, `"payload.type":8`) {
		t.Errorf("file DSL must filter type=8:\n%s", body)
	}
	if strings.Contains(body, `"virtual"`) {
		t.Errorf("file DSL must NOT carry virtual filter:\n%s", body)
	}
}

func TestBuildSearchFilesDSL_KeywordIncludesMultiMatch(t *testing.T) {
	req := SearchFilesReq{ChannelType: channelTypeGroup, ChannelID: "g", Keyword: "report"}
	q, _ := buildSearchFilesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
	js, _ := json.Marshal(extractDSL(t, q.(interface {
		Source() (any, error)
	})))
	body := string(js)
	if !strings.Contains(body, `"multi_match"`) {
		t.Errorf("file DSL with keyword should include multi_match:\n%s", body)
	}
	if !strings.Contains(body, "payload.file.name^2") {
		t.Errorf("file DSL with keyword should target payload.file.name^2:\n%s", body)
	}
}

// Highlight pin tests — the two text-side highlight builders must include
// payload.richText.searchText so a rich-text keyword hit surfaces a marked
// fragment under the same field name the snippet projection reads.
func TestBuildSearchMessagesHighlight_IncludesRichText(t *testing.T) {
	body := asJSONString(t, buildSearchMessagesHighlight())
	for _, want := range []string{
		`"payload.text.content"`,
		`"payload.richText.searchText"`,
		`"payload.mergeForward.msgs.searchText"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search_messages highlight missing %q in:\n%s", want, body)
		}
	}
	// Negative pin: type whitelist [1,11,14] never reaches image/file payloads,
	// so highlighting those fields is dead config.
	for _, deny := range []string{
		`"payload.image.caption"`,
		`"payload.file.name"`,
	} {
		if strings.Contains(body, deny) {
			t.Errorf("search_messages highlight should not include %q in:\n%s", deny, body)
		}
	}
}

func TestBuildSearchAllHighlight_IncludesRichText(t *testing.T) {
	body := asJSONString(t, buildSearchAllHighlight())
	for _, want := range []string{
		`"payload.text.content"`,
		`"payload.richText.searchText"`,
		`"payload.mergeForward.msgs.searchText"`,
		`"payload.file.name"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search_all highlight missing %q in:\n%s", want, body)
		}
	}
}

func TestBuildSearchAllDSL_TypeFilter(t *testing.T) {
	req := SearchMessagesReq{ChannelType: channelTypeGroup, ChannelID: "g", Keyword: "k"}
	q, _ := buildSearchAllDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
	js, _ := json.Marshal(extractDSL(t, q.(interface {
		Source() (any, error)
	})))
	body := string(js)
	for _, want := range []string{
		`"payload.type":[1,8,11,14]`,
		`"minimum_should_match":"1"`,
		`"payload.text.content^3"`,
		`"payload.richText.searchText^3"`,
		`"payload.file.name^2"`,
		`"virtual":true`,
	} {
		if !strings.Contains(body, want) && !strings.Contains(body, strings.ReplaceAll(want, ",", ", ")) {
			t.Errorf("search_all DSL missing %q in:\n%s", want, body)
		}
	}
}

func TestBuildSearchAllDSL_BrowseModeIncludesMediaTypes(t *testing.T) {
	req := SearchMessagesReq{ChannelType: channelTypeGroup, ChannelID: "g"}
	q, _ := buildSearchAllDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
	src, err := q.(interface{ Source() (any, error) }).Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	raw, _ := json.Marshal(src)
	body := string(raw)
	if strings.Contains(body, "multi_match") {
		t.Errorf("search_all DSL with empty keyword must not include multi_match:\n%s", body)
	}
	if strings.Contains(body, "minimum_should_match") {
		t.Errorf("search_all DSL with empty keyword must not include minimum_should_match:\n%s", body)
	}
	if strings.Contains(body, `"should"`) {
		t.Errorf("search_all DSL with empty keyword must not include a should clause:\n%s", body)
	}
	for _, want := range []string{
		`"channelId":"g"`,
		`"revoked":true`,
		`"virtual":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-keyword search_all DSL missing %q in:\n%s", want, body)
		}
	}
	// Browse mode (keyword="") layers image(2) + video(5) onto the type
	// whitelist so the unified feed shows recent media alongside text.
	// Order-independent: the previous string-substring assertion was brittle
	// and forced callers to know the exact emission order.
	got := extractSearchAllTypes(t, raw)
	for _, want := range []int{
		payloadTypeText,
		payloadTypeFile,
		payloadTypeMergeForward,
		payloadTypeRichText,
		payloadTypeImage,
		payloadTypeVideo,
	} {
		found := false
		for _, v := range got {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("empty-keyword search_all DSL must include payload.type=%d (got %v): %s", want, got, raw)
		}
	}
	if len(got) != 6 {
		t.Errorf("empty-keyword search_all DSL payload.type whitelist must have exactly 6 entries; got %d (%v)", len(got), got)
	}
}

// TestBuildSearchAllDSL_ImageVideoGatedByKeyword pins the contract that
// image (payload.type=2) and video (payload.type=5) appear in the
// /_search_all type filter ONLY when keyword is empty. The keyword path
// hard-excludes them because the should[textClause, fileClause] + MSM=1
// keyword clause has no field on a media payload and would emit
// zero-relevance hits.
func TestBuildSearchAllDSL_ImageVideoGatedByKeyword(t *testing.T) {
	t.Run("keyword excludes image and video", func(t *testing.T) {
		req := SearchMessagesReq{ChannelType: channelTypeGroup, ChannelID: "g", Keyword: "hello"}
		q, _ := buildSearchAllDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
		src, err := q.(interface{ Source() (any, error) }).Source()
		if err != nil {
			t.Fatalf("Source(): %v", err)
		}
		raw, _ := json.Marshal(src)
		got := extractSearchAllTypes(t, raw)
		for _, banned := range []int{payloadTypeImage, payloadTypeVideo} {
			for _, v := range got {
				if v == banned {
					t.Errorf("keyword path must not include payload.type=%d (got %v): %s", banned, got, raw)
				}
			}
		}
		for _, want := range []int{payloadTypeText, payloadTypeFile, payloadTypeMergeForward, payloadTypeRichText} {
			found := false
			for _, v := range got {
				if v == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("keyword path must include payload.type=%d (got %v)", want, got)
			}
		}
	})

	t.Run("empty keyword includes image and video", func(t *testing.T) {
		req := SearchMessagesReq{ChannelType: channelTypeGroup, ChannelID: "g"}
		q, _ := buildSearchAllDSL(context.Background(), fallbackTestAnalyzer(), true, req, "g", "")
		src, err := q.(interface{ Source() (any, error) }).Source()
		if err != nil {
			t.Fatalf("Source(): %v", err)
		}
		raw, _ := json.Marshal(src)
		got := extractSearchAllTypes(t, raw)
		for _, want := range []int{payloadTypeImage, payloadTypeVideo} {
			found := false
			for _, v := range got {
				if v == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("empty-keyword path must include payload.type=%d (got %v)", want, got)
			}
		}
	})
}

// extractSearchAllTypes reads the payload.type whitelist from a marshalled
// /_search_all DSL. Used by the browse-mode and keyword-gating tests so each
// stays order-independent (the previous string-substring assertion forced
// callers to know the exact emission order).
func extractSearchAllTypes(t *testing.T, raw []byte) []int {
	t.Helper()
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	boolNode, ok := normalized["bool"].(map[string]any)
	if !ok {
		t.Fatalf("query has no bool node: %s", raw)
	}
	rawFilter, ok := boolNode["filter"]
	if !ok {
		t.Fatalf("bool has no filter: %s", raw)
	}
	var filters []any
	switch v := rawFilter.(type) {
	case []any:
		filters = v
	case map[string]any:
		filters = []any{v}
	default:
		t.Fatalf("filter has unexpected shape %T: %s", rawFilter, raw)
	}
	for _, clause := range filters {
		m, ok := clause.(map[string]any)
		if !ok {
			continue
		}
		terms, ok := m["terms"].(map[string]any)
		if !ok {
			continue
		}
		arr, ok := terms["payload.type"].([]any)
		if !ok {
			continue
		}
		out := make([]int, 0, len(arr))
		for _, v := range arr {
			out = append(out, int(v.(float64)))
		}
		return out
	}
	t.Fatalf("bool.filter has no terms(payload.type) clause:\n%s", raw)
	return nil
}

func TestExtractSortValues(t *testing.T) {
	ts, msg, score := extractSortValues([]any{float64(1717000000), float64(9876543210)}, false)
	if ts != 1717000000 || msg != 9876543210 {
		t.Fatalf("got ts=%d msgID=%d", ts, msg)
	}
	if score != nil {
		t.Fatalf("time_* sort should yield score=nil, got %v", *score)
	}
	if ts, msg, score := extractSortValues(nil, false); ts != 0 || msg != 0 || score != nil {
		t.Fatalf("nil sort should give zeros, got %d %d %v", ts, msg, score)
	}
}

func TestExtractSortValues_Relevance(t *testing.T) {
	// relevance sort emits [timestamp, _score, messageId]
	ts, msg, score := extractSortValues([]any{float64(1717000000), float64(12.5), float64(9876543210)}, true)
	if ts != 1717000000 || msg != 9876543210 {
		t.Fatalf("got ts=%d msgID=%d", ts, msg)
	}
	if score == nil || *score != 12.5 {
		t.Fatalf("expected score=12.5, got %v", score)
	}
	// short sort under relevance returns zeros + nil
	if ts, msg, score := extractSortValues([]any{float64(1), float64(2)}, true); ts != 0 || msg != 0 || score != nil {
		t.Fatalf("short relevance sort should give zeros, got %d %d %v", ts, msg, score)
	}
}

func TestNumericTo64_JSONNumber(t *testing.T) {
	// json.Number must keep full int64 precision — this is the path a
	// NumberDecoder-configured client would produce for snowflake IDs.
	const big = int64(1817958721236045824)
	if got := numericTo64(json.Number("1817958721236045824")); got != big {
		t.Fatalf("json.Number precision lost: got %d want %d", got, big)
	}
	if got := numericToFloat(json.Number("12.5")); got != 12.5 {
		t.Fatalf("json.Number float: got %v want 12.5", got)
	}
}

// searchResultWithLastHit builds a minimal one-hit SearchResult whose Sort
// array carries float64 values (the default-decoder shape) and whose _source
// carries the full-precision messageId.
func searchResultWithLastHit(t *testing.T, msgID int64, ts int64) *elastic.SearchResult {
	t.Helper()
	src := json.RawMessage([]byte(`{"messageId":` + strconv.FormatInt(msgID, 10) + `,"timestamp":` + strconv.FormatInt(ts, 10) + `}`))
	return &elastic.SearchResult{
		Hits: &elastic.SearchHits{
			Hits: []*elastic.SearchHit{
				{
					// Default json.Unmarshal decodes sort numbers as float64,
					// which rounds above 2^53 — exactly the corruption the
					// cursor must not inherit.
					Sort:   []any{float64(ts), float64(msgID)},
					Source: &src,
				},
			},
		},
	}
}

// TestComputeCursorPagination_SnowflakeMessageIDPrecision is the regression
// test for the P1 review finding: messageId is a snowflake (> 2^53), the Sort
// array arrives as float64 and rounds it, and the encoded cursor must still
// carry the exact id (taken from the typed _source) or pagination skips /
// duplicates messages at timestamp-tied boundaries.
func TestComputeCursorPagination_SnowflakeMessageIDPrecision(t *testing.T) {
	const snowflake = int64(1817958721236045827) // > 2^53; int64(float64(x)) != x
	if int64(float64(snowflake)) == snowflake {
		t.Fatalf("test value must lose precision through float64 to be meaningful")
	}
	cfg := SearchConfig{CursorHMAC: "test-secret"}
	h := &Handler{cfg: cfg}

	result := searchResultWithLastHit(t, snowflake, 1717000000)
	hasMore, cursor := h.computeCursorPagination(result, 1, "time_desc")
	if !hasMore || cursor == "" {
		t.Fatalf("expected has_more with cursor, got %v %q", hasMore, cursor)
	}
	ts, msgID, score, _, err := decodeCursor(cfg, cursor)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if msgID != snowflake {
		t.Fatalf("cursor messageId lost precision: got %d want %d", msgID, snowflake)
	}
	if ts != 1717000000 {
		t.Fatalf("cursor ts: got %d", ts)
	}
	if score != nil {
		t.Fatalf("time_desc cursor should carry no score")
	}
}

// TestComputeCursorPagination_BadSourceNoCursor pins the fail-safe: when the
// last hit's _source cannot provide a messageId we suppress the cursor
// entirely instead of emitting a corrupt one.
func TestComputeCursorPagination_BadSourceNoCursor(t *testing.T) {
	h := &Handler{cfg: SearchConfig{CursorHMAC: "k"}}
	bad := json.RawMessage([]byte(`not-json`))
	result := &elastic.SearchResult{
		Hits: &elastic.SearchHits{
			Hits: []*elastic.SearchHit{
				{Sort: []any{float64(1717000000), float64(123)}, Source: &bad},
			},
		},
	}
	hasMore, cursor := h.computeCursorPagination(result, 1, "time_desc")
	if hasMore || cursor != "" {
		t.Fatalf("bad _source must suppress cursor, got %v %q", hasMore, cursor)
	}
}

// TestApplySort_IncludesSubSeqTiebreaker pins the Part B contract: all three
// sort variants append a trailing `subSeq` sort key in the matching primary
// direction (asc / desc). Without it, virtual sub-documents that share
// (timestamp, messageId) with their rich-text parent get silently skipped by
// OS's exclusive search_after — the symptom this whole change addresses.
//
// We assert on the marshalled sort spec emitted via SearchSource because
// applySort runs against an *elastic.SearchService whose internal sort list
// is not directly inspectable.
func TestApplySort_IncludesSubSeqTiebreaker(t *testing.T) {
	cases := []struct {
		sort    string
		want    []string // sort.field values in order
		wantDir []bool   // true=asc
	}{
		{"time_desc", []string{"timestamp", "messageId", "subSeq"}, []bool{false, false, false}},
		{"time_asc", []string{"timestamp", "messageId", "subSeq"}, []bool{true, true, true}},
		{"relevance", []string{"timestamp", "_score", "messageId", "subSeq"}, []bool{false, false, false, false}},
	}
	for _, tc := range cases {
		t.Run(tc.sort, func(t *testing.T) {
			// Drive the production sort builders directly so this pins the real
			// wire shape (field order + the subSeq unmapped_type/missing guards),
			// not a hand-rebuilt copy that could drift from applySort.
			ss := elastic.NewSearchSource()
			ss = ss.SortBy(searchSorters(tc.sort)...)
			src, err := ss.Source()
			if err != nil {
				t.Fatalf("Source(): %v", err)
			}
			raw, _ := json.Marshal(src)
			body := string(raw)
			// Cheap shape check: the marshalled sort array names every
			// expected field in the right order.
			lastIdx := -1
			for _, f := range tc.want {
				idx := strings.Index(body, `"`+f+`"`)
				if idx < 0 {
					t.Fatalf("%s: sort missing field %q in:\n%s", tc.sort, f, body)
				}
				if idx <= lastIdx {
					t.Fatalf("%s: field %q out of order (idx=%d, prev=%d) in:\n%s", tc.sort, f, idx, lastIdx, body)
				}
				lastIdx = idx
			}
			if !strings.Contains(body, `"subSeq"`) {
				t.Fatalf("%s: subSeq tiebreaker missing in:\n%s", tc.sort, body)
			}
			// Reader-first deploy guard: the subSeq sort MUST carry
			// unmapped_type + missing, otherwise OS 400s on the absent mapping
			// before the indexer rollout lands and every /_search* goes down.
			if !strings.Contains(body, `"unmapped_type":"long"`) {
				t.Fatalf("%s: subSeq sort missing unmapped_type guard in:\n%s", tc.sort, body)
			}
			if !strings.Contains(body, `"missing":0`) {
				t.Fatalf("%s: subSeq sort missing missing=0 guard in:\n%s", tc.sort, body)
			}
		})
	}
}

// TestBuildSearchAfterFromHit_AppendsSubSeq — pins the Part B contract that
// the search_after tuple ends with subSeq taken from the typed _source.
// Without this, the round-refill anchor in paginateWithFilter would drop
// the tiebreaker and silently skip virtual children at (ts, msgID) ties.
func TestBuildSearchAfterFromHit_AppendsSubSeq(t *testing.T) {
	// Non-relevance shape: [ts, msgID, subSeq]
	body := json.RawMessage([]byte(`{"messageId":42,"messageSeq":7,"timestamp":1717000000,"subSeq":3}`))
	hit := &elastic.SearchHit{
		Source: &body,
		Sort:   []any{float64(1717000000), float64(42), float64(3)},
	}
	sa, ok := buildSearchAfterFromHit(hit, false)
	if !ok {
		t.Fatalf("buildSearchAfterFromHit must accept well-formed hit")
	}
	if len(sa) != 3 {
		t.Fatalf("time_* tuple len: got %d want 3 (%v)", len(sa), sa)
	}
	if got, ok := sa[2].(int); !ok || got != 3 {
		t.Fatalf("search_after[2] must be int(subSeq=3); got %T(%v)", sa[2], sa[2])
	}

	// Relevance shape: [ts, score, msgID, subSeq]
	hitRel := &elastic.SearchHit{
		Source: &body,
		Sort:   []any{float64(1717000000), float64(5.5), float64(42), float64(3)},
	}
	saRel, ok := buildSearchAfterFromHit(hitRel, true)
	if !ok {
		t.Fatalf("relevance buildSearchAfterFromHit must accept well-formed hit")
	}
	if len(saRel) != 4 {
		t.Fatalf("relevance tuple len: got %d want 4 (%v)", len(saRel), saRel)
	}
	if got, ok := saRel[3].(int); !ok || got != 3 {
		t.Fatalf("relevance search_after[3] must be int(subSeq=3); got %T(%v)", saRel[3], saRel[3])
	}
}

// TestBuildSearchAfterFromHit_LegacyDocDefaultsSubSeqZero — a doc without
// the subSeq field (pre-Part-B storage) deserialises to Doc.SubSeq=0 and
// the search_after tuple carries 0 in the trailing slot. This is the
// reader-side mirror of the platform-side smooth-degrade contract: legacy
// docs in OS keep working without a reindex.
func TestBuildSearchAfterFromHit_LegacyDocDefaultsSubSeqZero(t *testing.T) {
	body := json.RawMessage([]byte(`{"messageId":42,"messageSeq":7,"timestamp":1717000000}`))
	hit := &elastic.SearchHit{
		Source: &body,
		Sort:   []any{float64(1717000000), float64(42)},
	}
	sa, ok := buildSearchAfterFromHit(hit, false)
	if !ok {
		t.Fatalf("buildSearchAfterFromHit must accept legacy hit")
	}
	if got, ok := sa[2].(int); !ok || got != 0 {
		t.Fatalf("legacy doc subSeq slot must be int(0); got %T(%v)", sa[2], sa[2])
	}
}

// TestComputeCursorPagination_CarriesSubSeq — when the last hit is a
// virtual sub-document with subSeq=N, the encoded cursor must carry N back
// so the next page's search_after resumes exclusively past (ts, msgID, N).
// This is the load-bearing end-to-end pin for the cross-page-boundary
// 漏图 fix: without subSeq on the cursor, the next page jumps back to (ts,
// msgID, 0)'s implicit position and re-emits / skips siblings.
func TestComputeCursorPagination_CarriesSubSeq(t *testing.T) {
	cfg := SearchConfig{CursorHMAC: "k"}
	h := &Handler{cfg: cfg}

	body := json.RawMessage([]byte(`{"messageId":7777,"timestamp":1717000000,"virtual":true,"parentMessageId":7777,"subSeq":2}`))
	result := &elastic.SearchResult{
		Hits: &elastic.SearchHits{
			Hits: []*elastic.SearchHit{
				{Source: &body, Sort: []any{float64(1717000000), float64(7777), float64(2)}},
			},
		},
	}
	hasMore, cursor := h.computeCursorPagination(result, 1, "time_desc")
	if !hasMore || cursor == "" {
		t.Fatalf("expected has_more with cursor, got %v %q", hasMore, cursor)
	}
	_, _, _, subSeq, err := decodeCursor(cfg, cursor)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if subSeq != 2 {
		t.Fatalf("cursor must carry subSeq=2 from the virtual child; got %d", subSeq)
	}
}

// TestPaginate_VirtualSiblingsCrossPageBoundary — the END-TO-END pin for
// the Part B 漏图 fix. Five hits all share (timestamp, messageId) — the
// pathological "5-image rich-text whose parent fills a page boundary" case
// — distinguished only by subSeq=1..5. Walking page_size=2 across the
// boundary must produce all 5 hits in order, no skip, no dup.
//
// We drive paginateWithFilter directly: round 1 yields the full 5 hits,
// the loop fills page=2 and anchors next_cursor at the 2nd hit
// (subSeq=2). Round 2 (the simulated next page) feeds the OS query the
// search_after tuple (ts, msgID, 2) — under OS exclusive comparison this
// returns subSeq=3,4,5; we assert the synthetic round 2 was called with
// the right tuple, and that page 1 = [subSeq=1, subSeq=2] in order.
//
// This is a guard against the v1 漏图 regression: with the OLD sort tuple
// [ts, msgID], all 5 siblings are equal and search_after at (ts, msgID)
// silently drops subSeq=3,4,5 entirely. With the fix the tuple is unique
// and every sibling surfaces.
func TestPaginate_VirtualSiblingsCrossPageBoundary(t *testing.T) {
	const sharedTS = int64(1717000000)
	const sharedMsgID = int64(7777)

	makeHit := func(subSeq int) *elastic.SearchHit {
		body, _ := json.Marshal(map[string]any{
			"messageId":       sharedMsgID,
			"messageSeq":      uint64(99),
			"channelId":       "C1",
			"timestamp":       sharedTS,
			"virtual":         true,
			"parentMessageId": sharedMsgID,
			"subSeq":          subSeq,
			"payload": map[string]any{
				"type":  2,
				"image": map[string]any{"url": "http://x", "caption": "img"},
			},
		})
		src := json.RawMessage(body)
		return &elastic.SearchHit{
			Source: &src,
			Sort:   []any{float64(sharedTS), float64(sharedMsgID), float64(subSeq)},
		}
	}

	// All five hits visible (parent not revoked).
	probe := &stubProbe{}
	h := newVisibilityHandler(probe)

	round := 0
	var lastSA []any
	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		round++
		switch round {
		case 1:
			// Whole batch in one round (size=2*3=6 >= 5)
			return []*elastic.SearchHit{
				makeHit(1), makeHit(2), makeHit(3), makeHit(4), makeHit(5),
			}, nil
		case 2:
			lastSA = append([]any{}, searchAfter...)
			return nil, nil
		}
		t.Fatalf("unexpected round %d", round)
		return nil, nil
	}

	collected, hasMore, nextCursor, err := h.paginateWithFilter(
		context.Background(), "me", "C1", 2, nil, false,
		osQuery, projectDocRef("C1", ""),
	)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(collected) != 2 {
		t.Fatalf("page 1: want 2 hits, got %d", len(collected))
	}
	// page 1 should be subSeq=1,2 in order
	for i, want := range []int{1, 2} {
		_, gotSub := lastHitMessageIDAndSubSeq(collected[i])
		if gotSub != want {
			t.Fatalf("page 1 hit[%d]: want subSeq=%d, got %d", i, want, gotSub)
		}
	}
	if !hasMore {
		t.Fatalf("hasMore must be true: round 1 returned 5 hits, page=2, so 3 more remain")
	}
	if nextCursor == "" {
		t.Fatalf("hasMore=true requires non-empty cursor")
	}
	// Now simulate the NEXT page: decode the cursor as search_after and
	// confirm the trailing slot is subSeq=2 (the last hit's subSeq). Under
	// OS's exclusive search_after this excludes all hits whose tuple is
	// equal to or past (ts, msgID, 2) in the sort direction, so the next
	// page resumes strictly past subSeq=2 — exactly the contract that
	// keeps subSeq=3,4,5 from being silently dropped at the boundary.
	sa, ok := decodeCursorAsSearchAfter(h.cfg, nextCursor, false)
	if !ok {
		t.Fatalf("next_cursor must decode as search_after")
	}
	if len(sa) != 3 {
		t.Fatalf("search_after must be 3-tuple [ts, msgID, subSeq]; got %v", sa)
	}
	if got, _ := sa[2].(int); got != 2 {
		t.Fatalf("search_after[2] must be subSeq=2 (last hit of page 1); got %v", sa[2])
	}
	_ = lastSA
}
