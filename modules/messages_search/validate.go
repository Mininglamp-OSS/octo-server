package messages_search

import (
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

const (
	maxKeywordLen = 64
	maxSenderIDs  = 50
	maxMemberUIDs = 50
	minPageSize   = 1
	maxPageSize   = 100
	defaultPage   = 20
)

// SearchFilters models the optional structured filters every endpoint shares.
type SearchFilters struct {
	SenderIDs  []string `json:"sender_ids,omitempty"`
	SentAtFrom string   `json:"sent_at_from,omitempty"`
	SentAtTo   string   `json:"sent_at_to,omitempty"`
}

// SearchMessagesReq is the request body for POST /v1/messages/_search.
//
// `Keyword` is optional; when empty the DSL drops the multi_match clause and
// the endpoint behaves as a time-ordered listing. To prevent an unconditional
// full-channel scan, an empty keyword still requires at least one effective
// filter (see validateSearchNotEmpty). `Sort` accepts time_desc (default) |
// time_asc | relevance — relevance requires a non-empty keyword. `PageSize` is
// normalised into [1, 100] with a default of 20.
type SearchMessagesReq struct {
	ChannelType uint8         `json:"channel_type"`
	ChannelID   string        `json:"channel_id"`
	Keyword     string        `json:"keyword,omitempty"`
	Filters     SearchFilters `json:"filters,omitempty"`
	Sort        string        `json:"sort,omitempty"`
	PageSize    int           `json:"page_size,omitempty"`
	Cursor      string        `json:"cursor,omitempty"`
}

// SearchMediaReq is the request body for POST /v1/messages/_search_media.
// Distinct from SearchMessagesReq because keyword must be empty (rejected
// with 400 if provided) and `relevance` sort is forbidden.
type SearchMediaReq struct {
	ChannelType uint8         `json:"channel_type"`
	ChannelID   string        `json:"channel_id"`
	Filters     SearchFilters `json:"filters,omitempty"`
	Sort        string        `json:"sort,omitempty"`
	PageSize    int           `json:"page_size,omitempty"`
	Cursor      string        `json:"cursor,omitempty"`
	Keyword     string        `json:"keyword,omitempty"` // must be empty
}

// SearchFilesReq is the request body for POST /v1/messages/_search_files.
// `Keyword` is optional — when empty the DSL drops the multi_match clause and
// becomes a pure type-filter listing.
type SearchFilesReq struct {
	ChannelType uint8         `json:"channel_type"`
	ChannelID   string        `json:"channel_id"`
	Keyword     string        `json:"keyword,omitempty"`
	Filters     SearchFilters `json:"filters,omitempty"`
	Sort        string        `json:"sort,omitempty"`
	PageSize    int           `json:"page_size,omitempty"`
	Cursor      string        `json:"cursor,omitempty"`
}

// SearchAllReq is the request body for POST /v1/messages/_search_all. Same
// shape as _search; keyword optional and gated identically.
type SearchAllReq = SearchMessagesReq

// SearchAroundReq is the request body for POST /v1/messages/_search_around.
// It locates a known anchor message and returns the chronological window
// around it (older + anchor + newer). There is no keyword, sort, or cursor:
// the window is anchored on a specific message_id and always returned in
// time_asc order so the client can render a contiguous conversation slice.
type SearchAroundReq struct {
	ChannelType     uint8         `json:"channel_type"`
	ChannelID       string        `json:"channel_id"`
	AnchorMessageID string        `json:"anchor_message_id"`
	Filters         SearchFilters `json:"filters,omitempty"`
	PageSize        int           `json:"page_size,omitempty"`
}

// validateBase covers the fields shared across all four endpoints:
// channel_type/id form, sender_ids count, time window order, page_size, sort
// enum, cursor signature.
func validateBase(c *wkhttp.Context, cfg SearchConfig, channelType uint8, channelID, sort, cursor string, filters SearchFilters, pageSize int, allowRelevance bool) (int, bool) {
	if !validChannelType(channelType) {
		respondValidation(c, "channel_type", "must be 1, 2, or 5")
		return 0, false
	}
	if channelID == "" {
		respondValidation(c, "channel_id", "required")
		return 0, false
	}
	if channelType == channelTypeThread && !strings.Contains(channelID, "____") {
		respondValidation(c, "channel_id", "thread channel_id must contain '____'")
		return 0, false
	}

	if len(filters.SenderIDs) > maxSenderIDs {
		respondValidationDetails(c, i18n.Details{
			"field":      "filters.sender_ids",
			"reason":     "too many",
			"max_length": maxSenderIDs,
		})
		return 0, false
	}

	from, fromOK := int64(0), filters.SentAtFrom == ""
	to, toOK := int64(0), filters.SentAtTo == ""
	if filters.SentAtFrom != "" {
		from, fromOK = parseSentAt(filters.SentAtFrom, true)
		if !fromOK {
			respondValidation(c, "filters.sent_at_from", "invalid time format")
			return 0, false
		}
	}
	if filters.SentAtTo != "" {
		to, toOK = parseSentAt(filters.SentAtTo, false)
		if !toOK {
			respondValidation(c, "filters.sent_at_to", "invalid time format")
			return 0, false
		}
	}
	if filters.SentAtFrom != "" && filters.SentAtTo != "" && from > to {
		respondValidation(c, "filters", "sent_at_from must be <= sent_at_to")
		return 0, false
	}

	switch sort {
	case "", "time_desc", "time_asc":
	case "relevance":
		if !allowRelevance {
			respondValidation(c, "sort", "relevance is not supported on this endpoint")
			return 0, false
		}
	default:
		respondValidation(c, "sort", "must be time_desc, time_asc, or relevance")
		return 0, false
	}

	if pageSize != 0 && (pageSize < minPageSize || pageSize > maxPageSize) {
		respondValidationDetails(c, i18n.Details{
			"field":      "page_size",
			"reason":     "out of range",
			"max_length": maxPageSize,
		})
		return 0, false
	}

	if cursor != "" {
		if _, _, _, _, err := decodeCursor(cfg, cursor); err != nil {
			respondValidation(c, "cursor", "malformed cursor")
			return 0, false
		}
	}

	page := pageSize
	if page == 0 {
		page = defaultPage
	}
	return page, true
}

// validateKeywordOptional accepts an empty keyword but still bounds length.
func validateKeywordOptional(c *wkhttp.Context, keyword string) bool {
	if keyword == "" {
		return true
	}
	if utf8.RuneCountInString(keyword) > maxKeywordLen {
		respondValidationDetails(c, i18n.Details{
			"field":      "keyword",
			"reason":     "too long",
			"max_length": maxKeywordLen,
		})
		return false
	}
	return true
}

// hasEffectiveFilters reports whether the structured filters carry at least one
// clause that survives DSL construction. It mirrors addCommonFilters' own
// emptiness handling so the validator and the query builder agree on what
// "has a filter" means:
//
//   - sender_ids is effective only if it contains a non-empty (trimmed) id.
//     `{"sender_ids":[""]}` is NOT effective — dsl.go::addCommonFilters drops
//     empty strings and would otherwise emit no terms clause, degenerating into
//     a full-channel scan. This is the documented bypass the guard must close.
//   - sent_at_from / sent_at_to is effective if either bound is set (trimmed).
//
// Keeping this in lockstep with addCommonFilters is the invariant: anything
// that would not produce an OS filter clause must not count as a filter here.
func hasEffectiveFilters(filters SearchFilters) bool {
	for _, s := range filters.SenderIDs {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	if strings.TrimSpace(filters.SentAtFrom) != "" {
		return true
	}
	if strings.TrimSpace(filters.SentAtTo) != "" {
		return true
	}
	return false
}

// validateSearchNotEmpty is the empty-search guard for the keyword-optional
// endpoints (_search / _search_all). With no keyword AND no effective filter,
// the query degenerates into an unconditional full-channel scan that can pin
// OpenSearch; reject it with 400 instead. keyword is assumed already trimmed by
// the caller.
func validateSearchNotEmpty(c *wkhttp.Context, keyword string, filters SearchFilters) bool {
	if keyword == "" && !hasEffectiveFilters(filters) {
		respondValidation(c, "keyword", "keyword or at least one filter is required")
		return false
	}
	return true
}

// validateKeywordMustBeEmpty enforces the `_search_media` rule that the
// keyword field is not accepted (the endpoint is a pure filter / list view).
func validateKeywordMustBeEmpty(c *wkhttp.Context, keyword string) bool {
	if keyword != "" {
		respondValidation(c, "keyword", "_search_media does not accept a keyword")
		return false
	}
	return true
}

func validChannelType(t uint8) bool {
	switch t {
	case channelTypePerson, channelTypeGroup, channelTypeThread:
		return true
	}
	return false
}
