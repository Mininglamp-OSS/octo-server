package search

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/namesearch"
)

type nameCursor struct {
	Query    string `json:"q"`
	Revision string `json:"r"`
	Offset   int    `json:"o"`
	Version  string `json:"v"`
}

func nameDigest(value interface{}) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func buildNamePage(uid, kind string, q nameQuery, set nameCandidateSet) (namePage, error) {
	if q.AllowedIDs != nil && q.AllowedIDsComplete != nil && !*q.AllowedIDsComplete {
		set.Complete = false
		set.Reasons = append(append([]string{}, set.Reasons...), "selection_pending")
	}
	hits, err := namesearch.RankCandidates(q.Keyword, set.Candidates)
	if err != nil {
		return namePage{}, err
	}
	items := make([]nameItem, 0, len(hits))
	allowed := map[string]bool{}
	if q.AllowedIDs != nil {
		for _, id := range *q.AllowedIDs {
			allowed[id] = true
		}
	}
	for _, h := range hits {
		if q.AllowedIDs != nil && !allowed[h.Candidate.ID] {
			continue
		}
		if kind == "contact" && q.ContactType != "" && q.ContactType != "all" && q.ContactType != h.Candidate.ContactType {
			continue
		}
		item := nameItem{Candidate: h.Candidate, DisplayName: h.Candidate.OriginalName, Matches: nameMatches{Original: []namesearch.Range{}, Remark: []namesearch.Range{}}}
		if strings.TrimSpace(h.Candidate.RemarkName) != "" {
			item.DisplayName = h.Candidate.RemarkName
		}
		if h.Original != nil {
			item.Matches.Original = h.Original.Ranges
		}
		if h.Remark != nil {
			item.Matches.Remark = h.Remark.Ranges
		}
		items = append(items, item)
	}
	condition := q
	condition.Cursor = ""
	condition.PageSize = 0
	if q.AllowedIDs != nil {
		ids := make([]string, 0, len(allowed))
		for id := range allowed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		condition.AllowedIDs = &ids
	}
	query := nameDigest([]interface{}{uid, kind, condition, namesearch.RulesVersion})
	revision := nameDigest([]interface{}{items, set.Complete, set.Reasons})
	offset := 0
	if q.Cursor != "" {
		var cursor nameCursor
		b, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(b, &cursor) != nil || cursor.Offset < 0 {
			return namePage{}, errNameCursorInvalid
		}
		if cursor.Query != query || cursor.Revision != revision || cursor.Version != namesearch.RulesVersion {
			return namePage{}, errNameQueryChanged
		}
		offset = cursor.Offset
		if offset > len(items) {
			return namePage{}, errNameCursorInvalid
		}
	}
	pageSize := q.PageSize
	if pageSize == 0 {
		pageSize = 50
	}
	if pageSize < 1 || pageSize > 100 {
		return namePage{}, errNameCursorInvalid
	}
	end := offset + pageSize
	if end > len(items) {
		end = len(items)
	}
	reasons := set.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	page := namePage{Items: items[offset:end], HasMore: end < len(items), Complete: set.Complete, IncompleteReasons: reasons, RulesVersion: namesearch.RulesVersion, ResultRevision: revision}
	if set.Complete {
		total := len(items)
		page.Total = &total
	}
	if page.HasMore {
		b, _ := json.Marshal(nameCursor{query, revision, end, namesearch.RulesVersion})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}
