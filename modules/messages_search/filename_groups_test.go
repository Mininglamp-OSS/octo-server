package messages_search

import (
	"context"
	"encoding/json"
	"github.com/olivere/elastic"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFilenameGroupedPreview(t *testing.T) {
	h := newHandlerForGlobalTests()
	for _, tc := range []struct{ name, keyword, want string }{
		{"ᾷ.pdf", "ᾷ", "<mark>ᾷ</mark>.pdf"},
		{"ᾷ.pdf", "ᾷ", "<mark>ᾷ</mark>.pdf"},
		{"\u0F43.pdf", "pdf", "\u0F43.<mark>pdf</mark>"},
		{"pre\u0F43.pdf", "\u0F42", "pre<mark>\u0F43</mark>.pdf"},
		{"\u0F43.pdf", "\u0FB7", "<mark>\u0F43</mark>.pdf"},
		{"ｶﾞｲﾄﾞ.pdf", "ガイド", "<mark>ｶﾞｲﾄﾞ</mark>.pdf"},
		{"ガイド.pdf", "ｶﾞｲﾄﾞ", "<mark>ガイド</mark>.pdf"},
		{"ﾊﾟｰﾂ.pdf", "パーツ", "<mark>ﾊﾟｰﾂ</mark>.pdf"},
		{"パーツ.pdf", "ﾊﾟｰﾂ", "<mark>パーツ</mark>.pdf"},
		{"Cafe\u0301.pdf", "CAFÉ", "<mark>Cafe\u0301</mark>.pdf"},
		{"Octo.pdf", "cto", "O<mark>cto</mark>.pdf"},
		{"😀ＯＣＴＯ<&.pdf", "octo", "😀<mark>ＯＣＴＯ</mark><&.pdf"},
		{"Straße.pdf", "STRASSE", "<mark>Straße</mark>.pdf"},
		{strings.Repeat("前", 200) + "ＯＣＴＯ" + strings.Repeat("后", 200), "octo", "…" + strings.Repeat("前", 16) + "<mark>ＯＣＴＯ</mark>" + strings.Repeat("后", 100) + "…"},
	} {
		t.Run(tc.keyword+tc.name[:4], func(t *testing.T) {
			tp := payloadTypeFile
			doc := Doc{MessageID: 1, MessageSeq: 1, ChannelID: "g", ChannelType: 2, Payload: &Payload{Type: &tp, File: &FilePayload{Name: tc.name}}}
			raw := json.RawMessage(mustJSON(doc))
			got := h.projectPreview(context.Background(), [][]*elastic.SearchHit{{{Source: &raw}}}, "me", tc.keyword)
			if len(got) != 1 || len(got[0]) != 1 || got[0][0].Snippet != tc.want {
				t.Fatalf("preview=%+v want=%s", got, tc.want)
			}
			if strings.Contains(tc.name, "前") && utf8.RuneCountInString(strings.NewReplacer("<mark>", "", "</mark>", "").Replace(got[0][0].Snippet)) > 122 {
				t.Fatal("unbounded preview")
			}
		})
	}
}
