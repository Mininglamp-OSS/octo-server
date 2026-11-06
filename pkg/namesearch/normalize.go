package namesearch

import (
	"strings"
	"sync"

	"github.com/longbridgeapp/opencc"
	"golang.org/x/text/width"
)

var converter struct {
	sync.Once
	cc  *opencc.OpenCC
	err error
}

type normalizedText struct {
	runes   []rune
	origins []int
}

// Conversion is per displayed character so its provenance survives width and
// simplified-character expansion. No whitespace or punctuation is removed here.
func normalize(raw string) (normalizedText, error) {
	converter.Do(func() { converter.cc, converter.err = opencc.New("t2s") })
	if converter.err != nil {
		return normalizedText{}, converter.err
	}
	var out normalizedText
	for i, r := range []rune(raw) {
		s, err := converter.cc.Convert(width.Fold.String(string(r)))
		if err != nil {
			return normalizedText{}, err
		}
		for _, nr := range []rune(strings.ToLower(s)) {
			out.runes = append(out.runes, nr)
			out.origins = append(out.origins, i)
		}
	}
	return out, nil
}

func rangesFromPositions(positions map[int]bool) []Range {
	result := make([]Range, 0)
	max := -1
	for p := range positions {
		if p > max {
			max = p
		}
	}
	for i := 0; i <= max; i++ {
		if !positions[i] {
			continue
		}
		start := i
		for i+1 <= max && positions[i+1] {
			i++
		}
		result = append(result, Range{start, i + 1})
	}
	return result
}
