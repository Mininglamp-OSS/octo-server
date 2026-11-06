package namesearch

import (
	"strings"
	"unicode"
)

func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func occurrences(text, query []rune, emit func(start, end int)) {
	if len(query) == 0 {
		return
	}
	for i := 0; i+len(query) <= len(text); i++ {
		if equalRunes(text[i:i+len(query)], query) {
			emit(i, i+len(query))
		}
	}
}

func MatchField(keyword, raw string, remark bool) (*FieldMatch, error) {
	if strings.TrimSpace(keyword) == "" || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	q, err := normalize(strings.TrimSpace(keyword))
	if err != nil {
		return nil, err
	}
	n, err := normalize(raw)
	if err != nil {
		return nil, err
	}
	if len(q.runes) == 0 {
		return nil, nil
	}
	var best *FieldMatch
	positions := map[int]bool{}
	emit := func(score, start int, pos []int) {
		if remark {
			score += 20
		}
		if best == nil || score > best.Score || score == best.Score && start < best.Start {
			best = &FieldMatch{Score: score, Start: start, Length: len(n.runes), Normalized: string(n.runes)}
		}
		for _, p := range pos {
			positions[p] = true
		}
	}
	collect := func(chars []rune, origins []int, exact, prefix, part int) bool {
		found := false
		occurrences(chars, q.runes, func(start, end int) {
			found = true
			score := part
			if start == 0 {
				score = prefix
				if end == len(chars) {
					score = exact
				}
			}
			emit(score, origins[start], origins[start:end])
		})
		return found
	}
	collect(n.runes, n.origins, 1000, 900, 800)
	allowPhonetic := true
	for _, r := range q.runes {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			allowPhonetic = false
			break
		}
	}
	if allowPhonetic {
		tokens, hasHan := syllables(n)
		if hasHan {
			// Phonetic normalization must not alter the literal matching above.
			q.runes = []rune(phoneticQuery(string(q.runes)))
			fullMatches(q.runes, tokens, emit)
			initial, initialOrigins := expression(tokens, true)
			collect(initial, initialOrigins, 500, 450, 400)
			mixedMatches(q.runes, tokens, emit)
		}
	}
	if best != nil {
		best.Ranges = rangesFromPositions(positions)
	}
	return best, nil
}
