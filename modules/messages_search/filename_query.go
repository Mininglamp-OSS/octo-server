package messages_search

import (
	"context"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/olivere/elastic"
	"golang.org/x/text/cases"
	"golang.org/x/text/width"
)

// Keep this normalization in sync with octo-search-indexer/internal/esindex.
// Do not use NFKC: compatibility characters beyond width are significant.
func foldFilename(text string) string {
	return cases.Fold().String(width.Fold.String(text))
}

func buildFileKeywordClause(ctx context.Context, analyzer tokenAnalyzer, strip bool, keyword string) (elastic.Query, error) {
	effective, useMSM := keyword, true
	var err error
	if strip {
		effective, useMSM, err = AnalyzeKeyword(ctx, analyzer, keyword)
	}
	return buildFileKeywordClauseFromAnalyzed(keyword, effective, useMSM), err
}

func buildFileKeywordClauseFromAnalyzed(keyword, effective string, useMSM bool) elastic.Query {
	// The indexer stores one token per Unicode character, INCLUDING spaces and
	// punctuation. A zero-slop phrase is a literal substring, not phrase syntax
	// supplied by the user. This uses postings/positions, never a _source scan.
	literal := elastic.NewMatchPhraseQuery("payload.file.nameLiteral", foldFilename(strings.TrimSpace(keyword))).Slop(0)
	fulltext := buildKeywordClauseFromAnalyzed(effective, useMSM,
		"payload.file.name^2", "payload.file.caption", "payload.file.content")
	// Preserve the existing cross-field query and its score. Require at least
	// one caption/body token so tokenization of the filename alone cannot bypass
	// T1 (e.g. punctuation or spaces). Filename + body joint matches still work.
	bodyEvidence := elastic.NewMultiMatchQuery(effective, "payload.file.caption", "payload.file.content").
		Type("cross_fields").MinimumShouldMatch("1")
	// Retain existing scores for existing eligible matches, including literal
	// names that the old analyzer could find. Newly supported substrings get the
	// existing filename boost. Do not add scores when both branches match.
	allowed := elastic.NewBoolQuery().Should(literal, bodyEvidence).MinimumShouldMatch("1")
	textWithEvidence := elastic.NewBoolQuery().Must(fulltext).Filter(allowed)
	newLiteral := elastic.NewBoolQuery().Filter(literal).MustNot(fulltext)
	literalFallback := elastic.NewConstantScoreQuery(newLiteral).Boost(2)
	return elastic.NewDisMaxQuery().Query(textWithEvidence, literalFallback)
}

// literalFilenameHighlight maps folded matches back onto the original filename.
// It is used only on the returned page, never to filter or count search results.
func literalFilenameHighlight(name, keyword string) string {
	return literalFilenameHighlightWindow(name, keyword, 0)
}

// A bounded original-text window keeps the first match visible in group previews.
func literalFilenameHighlightWindow(name, keyword string, maxRunes int) string {
	needle := foldFilename(strings.TrimSpace(keyword))
	if needle == "" {
		return ""
	}
	var folded strings.Builder
	var starts, ends []int
	for start, r := range name {
		piece := foldFilename(string(r))
		folded.WriteString(piece)
		for range len(piece) {
			starts = append(starts, start)
			ends = append(ends, start+len(string(r)))
		}
	}
	text := folded.String()
	type span struct{ start, end int }
	var spans []span
	for from := 0; from <= len(text)-len(needle); {
		at := strings.Index(text[from:], needle)
		if at < 0 {
			break
		}
		at += from
		s := span{starts[at], ends[at+len(needle)-1]}
		if len(spans) > 0 && s.start <= spans[len(spans)-1].end {
			spans[len(spans)-1].end = s.end
		} else {
			spans = append(spans, s)
		}
		from = at + 1 // include overlapping occurrences
	}
	if len(spans) == 0 {
		return ""
	}
	start, end := 0, len(name)
	if maxRunes > 0 {
		runes := []rune(name)
		first := utf8.RuneCountInString(name[:spans[0].start])
		left := max(0, first-16)
		right := min(len(runes), left+maxRunes)
		start, end = len(string(runes[:left])), len(string(runes[:right]))
	}
	var out strings.Builder
	if start > 0 {
		out.WriteString("…")
	}
	last := start
	for _, s := range spans {
		if s.end <= start || s.start >= end {
			continue
		}
		s.start, s.end = max(start, s.start), min(end, s.end)
		out.WriteString(html.EscapeString(name[last:s.start]))
		out.WriteString("<mark>")
		out.WriteString(html.EscapeString(name[s.start:s.end]))
		out.WriteString("</mark>")
		last = s.end
	}
	out.WriteString(html.EscapeString(name[last:end]))
	if end < len(name) {
		out.WriteString("…")
	}
	return out.String()
}
