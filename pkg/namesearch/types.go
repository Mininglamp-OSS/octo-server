// Package namesearch implements S14 name rules independently of HTTP and storage.
package namesearch

const RulesVersion = "s14-v1"

// Range uses Unicode code points in the unchanged display text, not UTF-16 units.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Candidate struct {
	ParentID     string `json:"parent_id,omitempty"`
	Archived     bool   `json:"archived,omitempty"`
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ContactType  string `json:"contact_type,omitempty"`
	BotKind      string `json:"bot_kind,omitempty"`
	OriginalName string `json:"original_name"`
	RemarkName   string `json:"remark_name"`
}

// Ranges includes ALL matches, whereas the other fields describe the best match.
type FieldMatch struct {
	Score      int
	Start      int
	Length     int
	Normalized string
	Ranges     []Range
}

type RankedHit struct {
	Candidate Candidate
	Original  *FieldMatch
	Remark    *FieldMatch
	Best      FieldMatch
}
