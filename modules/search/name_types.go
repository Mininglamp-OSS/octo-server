package search

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/namesearch"
)

var (
	errNameQueryChanged     = errors.New("name query changed")
	errNameCursorInvalid    = errors.New("invalid name cursor")
	errNameForbidden        = errors.New("name search forbidden")
	errNameScopeUnsupported = errors.New("name search scope is not configured")
)

type nameQuery struct {
	IncludeArchived bool    `json:"include_archived,omitempty"`
	Keyword         string  `json:"keyword"`
	SpaceID         *string `json:"space_id"`
	Entry           string  `json:"entry"`
	Scope           string  `json:"scope"`
	ContactType     string  `json:"contact_type"`
	ChannelID       string  `json:"channel_id,omitempty"`
	ChannelType     int     `json:"channel_type,omitempty"`
	PageSize        int     `json:"page_size"`
	Cursor          string  `json:"cursor,omitempty"`
	// A narrowing set from an entry's existing selectable roster. It never
	// grants visibility or action permission; R1/R2 still run first. nil = all,
	// [] = none. Apply before pagination, not to one already-truncated page.
	AllowedIDs         *[]string `json:"allowed_ids,omitempty"`
	AllowedIDsComplete *bool     `json:"allowed_ids_complete,omitempty"`
}

func (q nameQuery) space() string {
	if q.SpaceID == nil {
		return ""
	}
	return *q.SpaceID
}
func (q *nameQuery) validate(kind string) string {
	if strings.TrimSpace(q.Keyword) == "" || !utf8.ValidString(q.Keyword) {
		return "keyword"
	}
	max := 100
	switch q.Entry {
	case "global":
		max = 64
	case "address_book", "name_selector":
	default:
		return "entry"
	}
	if utf8.RuneCountInString(q.Keyword) > max {
		return "keyword"
	}
	if q.Scope == "" {
		q.Scope = "directory"
	}

	switch q.Scope {
	case "directory":
		if kind != "contact" && kind != "group" {
			return "scope"
		}
	case "member", "mention":
		if kind != "contact" {
			return "scope"
		}
		if q.ChannelID == "" || q.ChannelType != 2 {
			return "channel_id"
		}
	case "forward":
		if kind != "thread" {
			return "scope"
		}
	case "thread_filter":
		if kind != "thread" || q.ChannelID == "" || q.ChannelType != 2 {
			return "channel_id"
		}
	case "summary_member":
		if kind != "contact" {
			return "scope"
		}
		if q.ChannelID != "" && q.ChannelType != 2 && q.ChannelType != 5 {
			return "channel_id"
		}
	case "summary_source":
		if kind != "contact" && kind != "group" && kind != "thread" {
			return "scope"
		}
	default:
		return "scope"
	}
	if q.ChannelID != "" && q.Scope != "member" && q.Scope != "mention" && q.Scope != "thread_filter" && q.Scope != "summary_member" {
		return "channel_id"
	}
	if q.ChannelID == "" && q.ChannelType != 0 {
		return "channel_id"
	}
	if q.IncludeArchived && q.Scope != "thread_filter" && !(q.Scope == "summary_source" && kind == "thread") {
		return "include_archived"
	}
	if q.ContactType == "" {
		q.ContactType = "all"
	}
	if q.ContactType != "all" && q.ContactType != "human" && q.ContactType != "ai" {
		return "contact_type"
	}
	if kind != "contact" && q.ContactType != "all" {
		return "contact_type"
	}
	if q.PageSize == 0 {
		q.PageSize = 50
	}
	if q.PageSize < 1 || q.PageSize > 100 {
		return "page_size"
	}
	if q.AllowedIDs == nil && q.AllowedIDsComplete != nil && !*q.AllowedIDsComplete {
		return "allowed_ids"
	}
	if len(q.Cursor) > 1024 {
		return "cursor"
	}
	return ""
}

type nameCandidateSet struct {
	Candidates []namesearch.Candidate
	Complete   bool
	Reasons    []string
}
type nameMatches struct {
	Original []namesearch.Range `json:"original"`
	Remark   []namesearch.Range `json:"remark"`
}
type nameItem struct {
	namesearch.Candidate
	DisplayName string      `json:"display_name"`
	Matches     nameMatches `json:"matches"`
}
type namePage struct {
	Items             []nameItem `json:"items"`
	NextCursor        string     `json:"next_cursor"`
	HasMore           bool       `json:"has_more"`
	Complete          bool       `json:"complete"`
	Total             *int       `json:"total"`
	IncompleteReasons []string   `json:"incomplete_reasons"`
	RulesVersion      string     `json:"rules_version"`
	ResultRevision    string     `json:"result_revision"`
}
type nameSearchService struct {
	load func(context.Context, string, string, nameQuery) (nameCandidateSet, error)
}

func (s nameSearchService) search(ctx context.Context, uid, kind string, q nameQuery) (namePage, error) {
	set, err := s.load(ctx, uid, kind, q)
	if err != nil {
		return namePage{}, err
	}
	return buildNamePage(uid, kind, q, set)
}
