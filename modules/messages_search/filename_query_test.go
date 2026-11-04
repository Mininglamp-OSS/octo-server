package messages_search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/olivere/elastic"
)

func TestLiteralFilenameHighlight(t *testing.T) {
	for _, tc := range []struct{ name, keyword, want string }{
		{"ｶﾞｲﾄﾞ.pdf", "ガイド", "<mark>ｶﾞｲﾄﾞ</mark>.pdf"},
		{"ガイド.pdf", "ｶﾞｲﾄﾞ", "<mark>ガイド</mark>.pdf"},
		{"ﾊﾟｰﾂ.pdf", "パーツ", "<mark>ﾊﾟｰﾂ</mark>.pdf"},
		{"パーツ.pdf", "ﾊﾟｰﾂ", "<mark>パーツ</mark>.pdf"},
		{"Cafe\u0301.pdf", "CAFÉ", "<mark>Cafe\u0301</mark>.pdf"},
		{"ＯＣＴＯ方案ｖ１．２.pdf", " octo ", "<mark>ＯＣＴＯ</mark>方案ｖ１．２.pdf"},
		{"Octo方案v1.2.pdf", "v12", ""},
		{"ＯＣＴＯ方案ｖ１．２.pdf", "v1.2", "ＯＣＴＯ方案<mark>ｖ１．２</mark>.pdf"},
		{"排期与评审.pdf", "排期 评审", ""},
		{"排期  评审.pdf", "排期 评审", ""},
		{"排期 评审.pdf", "排期 评审", "<mark>排期 评审</mark>.pdf"},
		{"100%_完成*?.pdf", "*?", "100%_完成<mark>*?</mark>.pdf"},
		{"plain.pdf", "*", ""},
		{"😀Straße-STRASSE.pdf", "STRASSE", "😀<mark>Straße</mark>-<mark>STRASSE</mark>.pdf"},
		{"banana.pdf", "ana", "b<mark>anana</mark>.pdf"},
		{"<mark>&ＯＣＴＯ.pdf", "octo", "&lt;mark&gt;&amp;<mark>ＯＣＴＯ</mark>.pdf"},
		{"ｶﾀｶﾅ.pdf", "カタカナ", "<mark>ｶﾀｶﾅ</mark>.pdf"},
		{"①繁體.pdf", "1", ""},
		{"繁體.pdf", "繁体", ""},
		{"report.pdf", "   ", ""},
	} {
		t.Run(tc.name+"/"+tc.keyword, func(t *testing.T) {
			if got := literalFilenameHighlight(tc.name, tc.keyword); got != tc.want {
				t.Fatalf("highlight = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestFileKeywordClause_AnalyzeAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		strip bool
		err   error
		calls int
	}{
		{"enabled", true, nil, 1}, {"disabled", false, nil, 0},
		{"unavailable", true, errors.New("unavailable"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &countingAnalyzer{inner: stubAnalyzer{tokens: []string{"octo", "v1.2"}, err: tc.err}}
			q, err := buildFileKeywordClause(context.Background(), a, tc.strip, "ＯＣＴＯ v1.2")
			if a.calls != tc.calls || !errors.Is(err, tc.err) {
				t.Fatalf("calls=%d err=%v", a.calls, err)
			}
			body := marshalDSL(t, q)
			for _, want := range []string{`"payload.file.nameLiteral"`, `"query":"octo v1.2"`, `"slop":0`, `"payload.file.name^2"`, `"payload.file.caption"`, `"payload.file.content"`, `"minimum_should_match":"75%"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, `"wildcard"`) || strings.Contains(body, `"script"`) {
				t.Fatal("literal filename search must use indexed positions")
			}
		})
	}
}

func TestFileNameHighlight_OriginalTextAndNoFabricatedBody(t *testing.T) {
	fileType := payloadTypeFile
	doc := Doc{Payload: &Payload{Type: &fileType, File: &FilePayload{Name: "😀ＯＣＴＯ &.pdf"}}}
	h := &Handler{}
	file := h.singleFileHit(doc, "g", channelTypeGroup, nil, "octo")
	if file.NameHighlight != "😀<mark>ＯＣＴＯ</mark> &amp;.pdf" || file.ContentSnippet != "" {
		t.Fatalf("file=%+v", file)
	}
	mixed := h.singleSearchAllHit(doc, "g", channelTypeGroup, nil, "octo")
	if mixed.File == nil || mixed.File.NameHighlight != file.NameHighlight {
		t.Fatalf("mixed=%+v", mixed)
	}
}

func TestFilenameHighlight_GlobalAndChannelParity(t *testing.T) {
	h := newHandlerForGlobalTests()
	fileType := payloadTypeFile
	doc := Doc{MessageID: 1, MessageSeq: 1, ChannelID: "g", ChannelType: uint32(channelTypeGroup), Payload: &Payload{Type: &fileType, File: &FilePayload{Name: "ＯＣＴＯ &.pdf"}}}
	raw := json.RawMessage(mustJSON(doc))
	hits := []*elastic.SearchHit{{Source: &raw}}
	files := h.buildFileHits(context.Background(), hits, SearchFilesReq{Keyword: "octo", ChannelID: "g", ChannelType: channelTypeGroup}, "me")
	global := h.buildGlobalFileHits(context.Background(), hits, "me", "octo")
	all := h.buildSearchAllHits(context.Background(), hits, SearchAllReq{Keyword: "octo", ChannelID: "g", ChannelType: channelTypeGroup}, "me")
	globalAll := h.buildGlobalSearchAllHits(context.Background(), hits, "me", "octo")
	for _, file := range []FileHit{files[0], global[0], *all[0].File, *globalAll[0].File} {
		if file.NameHighlight != "<mark>ＯＣＴＯ</mark> &amp;.pdf" || file.ContentSnippet != "" || file.ChannelID != "g" || file.MessageID != "1" {
			t.Fatalf("incorrect projection: %+v", file)
		}
	}
}

func TestKanaFilenameHitHighlights(t *testing.T) {
	for _, tc := range []struct{ name, query, marked string }{
		{"ｶﾞｲﾄﾞ.pdf", "ガイド", "<mark>ｶﾞｲﾄﾞ</mark>.pdf"},
		{"ガイド.pdf", "ｶﾞｲﾄﾞ", "<mark>ガイド</mark>.pdf"},
		{"ﾊﾟｰﾂ.pdf", "パーツ", "<mark>ﾊﾟｰﾂ</mark>.pdf"},
		{"パーツ.pdf", "ﾊﾟｰﾂ", "<mark>パーツ</mark>.pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandlerForGlobalTests()
			tp := payloadTypeFile
			doc := Doc{MessageID: 1, ChannelID: "g", ChannelType: 2, Payload: &Payload{Type: &tp, File: &FilePayload{Name: tc.name}}}
			raw := json.RawMessage(mustJSON(doc))
			hits := []*elastic.SearchHit{{Source: &raw}}
			files := h.buildFileHits(context.Background(), hits, SearchFilesReq{Keyword: tc.query, ChannelID: "g", ChannelType: 2}, "me")
			global := h.buildGlobalFileHits(context.Background(), hits, "me", tc.query)
			all := h.buildSearchAllHits(context.Background(), hits, SearchAllReq{Keyword: tc.query, ChannelID: "g", ChannelType: 2}, "me")
			globalAll := h.buildGlobalSearchAllHits(context.Background(), hits, "me", tc.query)
			for _, f := range []FileHit{files[0], global[0], *all[0].File, *globalAll[0].File} {
				if f.NameHighlight != tc.marked || f.ContentSnippet != "" {
					t.Fatalf("incorrect highlight: %+v", f)
				}
			}
		})
	}
}
