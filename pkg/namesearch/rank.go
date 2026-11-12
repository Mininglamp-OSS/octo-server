package namesearch

import (
	"fmt"
	"sort"
)

func compareField(a, b FieldMatch) int {
	if a.Score != b.Score {
		if a.Score > b.Score {
			return -1
		}
		return 1
	}
	if a.Start != b.Start {
		if a.Start < b.Start {
			return -1
		}
		return 1
	}
	if a.Length != b.Length {
		if a.Length < b.Length {
			return -1
		}
		return 1
	}
	// UTF-8 lexicographic order equals Unicode scalar order for valid strings.
	if a.Normalized < b.Normalized {
		return -1
	}
	if a.Normalized > b.Normalized {
		return 1
	}
	return 0
}

func RankCandidates(keyword string, items []Candidate) ([]RankedHit, error) {
	hits := make([]RankedHit, 0)
	for _, item := range items {
		if item.Kind == "contact" && item.ContactType != "human" && item.ContactType != "ai" {
			return nil, fmt.Errorf("unconfirmed contact type for %q", item.ID)
		}
		original, err := MatchField(keyword, item.OriginalName, false)
		if err != nil {
			return nil, err
		}
		remark, err := MatchField(keyword, item.RemarkName, true)
		if err != nil {
			return nil, err
		}
		if original == nil && remark == nil {
			continue
		}
		best := original
		if best == nil || remark != nil && compareField(*remark, *best) < 0 {
			best = remark
		}
		hits = append(hits, RankedHit{item, original, remark, *best})
	}
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Candidate.Kind == "contact" && b.Candidate.Kind == "contact" && a.Candidate.ContactType != b.Candidate.ContactType {
			return a.Candidate.ContactType == "human"
		}
		if cmp := compareField(a.Best, b.Best); cmp != 0 {
			return cmp < 0
		}
		return a.Candidate.ID < b.Candidate.ID
	})
	return hits, nil
}
