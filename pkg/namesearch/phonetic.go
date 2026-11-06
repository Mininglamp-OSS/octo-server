package namesearch

import (
	"strings"
	"unicode"

	pinyin "github.com/mozillazg/go-pinyin"
)

type syllable struct {
	text   string
	raw    rune
	origin int
	han    bool
}

// A single deterministic reading, not an expansion of every pronunciation.
var readings = map[string][]string{
	"单明": {"shan", "ming"}, "曾华": {"zeng", "hua"},
	"重庆": {"chong", "qing"}, "长沙": {"chang", "sha"}, "许可": {"xu", "ke"},
}

func phoneticQuery(q string) string {
	return strings.ReplaceAll(q, "ü", "v")
}

func spellings(tok syllable) []string {
	result := []string{tok.text}
	// Compatibility belongs to a Chinese syllable, never a literal Latin suffix.
	if tok.han && len(tok.text) >= 2 && strings.ContainsRune("jqxy", rune(tok.text[0])) && tok.text[1] == 'u' {
		result = append(result, tok.text[:1]+"v"+tok.text[2:])
	}
	return result
}

func fullMatches(q []rune, tokens []syllable, emit func(int, int, []int)) {
	for start := range tokens {
		for _, first := range spellings(tokens[start]) {
			for offset := range []rune(first) {
				type state struct{ token, input int }
				seen := map[state]bool{}
				var visit func(token, input int, word []rune)
				visit = func(token, input int, word []rune) {
					used := len(word)
					if len(q)-input < used {
						used = len(q) - input
					}
					if used == 0 || !equalRunes(q[input:input+used], word[:used]) {
						return
					}
					if input+used == len(q) {
						score := 600
						if start == 0 && offset == 0 {
							score = 650
							if token == len(tokens)-1 && used == len(word) {
								score = 700
							}
						}
						pos := make([]int, 0, token-start+1)
						for i := start; i <= token; i++ {
							pos = append(pos, tokens[i].origin)
						}
						emit(score, tokens[start].origin, pos)
						return
					}
					next := state{token + 1, input + used}
					if token+1 >= len(tokens) || seen[next] {
						return
					}
					seen[next] = true
					for _, s := range spellings(tokens[token+1]) {
						visit(token+1, input+used, []rune(s))
					}
				}
				visit(start, 0, []rune(first)[offset:])
			}
		}
	}
}

func syllables(text normalizedText) ([]syllable, bool) {
	args := pinyin.NewArgs()
	args.Style = pinyin.Normal
	args.Heteronym = false
	args.Fallback = func(r rune, _ pinyin.Args) []string { return []string{string(r)} }
	var tokens []syllable
	hasHan := false
	for i, r := range text.runes {
		if unicode.IsSpace(r) || r == '.' || r == '-' {
			continue
		}
		han := unicode.Is(unicode.Han, r)
		s := string(r)
		if han {
			hasHan = true
			py := pinyin.SinglePinyin(r, args)
			if len(py) > 0 {
				s = py[0]
			}
		}
		tokens = append(tokens, syllable{phoneticQuery(strings.ToLower(s)), r, text.origins[i], han})
	}
	for word, py := range readings {
		chars := []rune(word)
		for i := 0; i+len(chars) <= len(tokens); i++ {
			ok := true
			for j, r := range chars {
				if tokens[i+j].raw != r {
					ok = false
					break
				}
			}
			if ok {
				for j, p := range py {
					tokens[i+j].text = p
				}
			}
		}
	}
	return tokens, hasHan
}

func expression(tokens []syllable, initial bool) ([]rune, []int) {
	var chars []rune
	var origins []int
	for _, tok := range tokens {
		r := []rune(tok.text)
		if initial && tok.han && len(r) > 0 {
			r = r[:1]
		}
		for _, c := range r {
			chars = append(chars, c)
			origins = append(origins, tok.origin)
		}
	}
	return chars, origins
}

const (
	formHan     = 1
	formFull    = 2
	formInitial = 4
	formRaw     = 8
)

// Mixed expressions consume consecutive source characters. State deduplication
// avoids materializing an exponential list of every spelling of a name.
func mixedMatches(q []rune, tokens []syllable, emit func(score, start int, positions []int)) {
	type state struct{ token, input, forms int }
	for start := range tokens {
		seen := map[state]bool{}
		var visit func(state)
		visit = func(st state) {
			if seen[st] || st.token >= len(tokens) {
				return
			}
			seen[st] = true
			tok := tokens[st.token]
			type option struct {
				text []rune
				form int
			}
			options := []option{{[]rune(tok.text), formRaw}}
			if tok.han {
				options = []option{{[]rune{tok.raw}, formHan}, {[]rune(tok.text)[:1], formInitial}}
				for _, s := range spellings(tok) {
					options = append(options, option{[]rune(s), formFull})
				}
			}
			for _, opt := range options {
				remaining := q[st.input:]
				used := len(opt.text)
				if used > len(remaining) {
					used = len(remaining)
				}
				if used == 0 || !equalRunes(remaining[:used], opt.text[:used]) {
					continue
				}
				// Only a full syllable may be partially entered, at query end.
				partial := used < len(opt.text)
				if partial && opt.form != formFull {
					continue
				}
				forms := st.forms | opt.form
				if st.input+used == len(q) {
					mixed := forms&(formHan|formFull) != 0 && forms&(forms-1) != 0
					// A pure initial expression must not gain mixed points for this span.
					// Other spans can still independently match a mixed expression.
					initial, _ := expression(tokens[start:st.token+1], true)
					if mixed && !equalRunes(q, initial) {
						score := 600
						if start == 0 {
							score = 650
							if st.token == len(tokens)-1 && !partial {
								score = 700
							}
						}
						pos := make([]int, 0, st.token-start+1)
						for j := start; j <= st.token; j++ {
							pos = append(pos, tokens[j].origin)
						}
						emit(score, tokens[start].origin, pos)
					}
				} else if !partial {
					visit(state{st.token + 1, st.input + used, forms})
				}
			}
		}
		visit(state{token: start})
	}
}
