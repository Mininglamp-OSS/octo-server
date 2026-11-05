package messages_search

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/olivere/elastic"
)

// Run against an isolated OpenSearch + IK instance and the companion indexer's
// actual mapping. Only the uniquely named test index is created/deleted.
func TestF3FilenameOpenSearch(t *testing.T) {
	endpoint, mappingPath := os.Getenv("F3_OPENSEARCH_URL"), os.Getenv("F3_INDEXER_MAPPING")
	if endpoint == "" || mappingPath == "" {
		t.Skip("set F3_OPENSEARCH_URL and F3_INDEXER_MAPPING for cross-repository OpenSearch acceptance")
	}
	client, err := elastic.NewClient(elastic.SetURL(endpoint), elastic.SetSniff(false), elastic.SetHealthcheck(false))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	index := fmt.Sprintf("test-f3-filename-%d", time.Now().UnixNano())
	mapping, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.CreateIndex(index).BodyString(string(mapping)).Do(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = client.DeleteIndex(index).Do(cleanup)
	})
	fixtures := []struct {
		id, name, caption, body, channel, space string
		revoked                                 bool
		channelType                             int
	}{
		{"1", "ＯＣＴＯ方案ｖ１．２.pdf", "", "", "g", "s", false, 2},
		{"2", "Octo方案v12.pdf", "", "", "g", "s", false, 2},
		{"3", "report.pdf", "", "项目排期已更新，下周完成方案评审", "g", "s", false, 2},
		{"4", "排期 评审.pdf", "", "", "g", "s", false, 2},
		{"5", "排期与评审.pdf", "", "", "g", "s", false, 2},
		{"6", "排期.pdf", "评审", "", "g", "s", false, 2},
		{"7", "100%_完成*?.pdf", "", "", "g", "s", false, 2},
		{"8", "😀Straße.pdf", "", "", "g", "s", false, 2},
		{"9", "ＯＣＴＯ方案ｖ１．２.pdf", "", "", "g", "s", false, 2}, // same name, different attachment
		{"10", "ＯＣＴＯ方案ｖ１．２.pdf", "", "", "foreign", "s", false, 2},
		{"11", "ＯＣＴＯ方案ｖ１．２.pdf", "", "", "g", "s", true, 2},
		{"12", "unrelated.pdf", "octo", "", "g", "s", false, 2},
		{"13", "ＯＣＴＯ.pdf", "", "ＯＣＴＯ", "g", "s", false, 2}, // two fields, one result
		{"14", "ＯＣＴＯ.pdf", "", "", "dm", "other-space", false, 1},
		{"15", "ＯＣＴＯ.pdf", "", "", "dm", "s", false, 1},
		{"16", "排期  评审.pdf", "", "", "g", "s", false, 2},
		{"17", "ｶﾀｶﾅ.pdf", "", "", "g", "s", false, 2},
		{"18", "繁體①.pdf", "", "", "g", "s", false, 2},
		{"19", "emoji😀.pdf", "", "", "g", "s", false, 2},
		{"20", "ｶﾞｲﾄﾞ.pdf", "", "", "g", "s", false, 2},
		{"21", "ガイド.pdf", "", "", "g", "s", false, 2},
		{"22", "ﾊﾟｰﾂ.pdf", "", "", "g", "s", false, 2},
		{"23", "パーツ.pdf", "", "", "g", "s", false, 2},
		{"24", "ᾷ.pdf", "", "", "g", "s", false, 2},
		{"25", "ᾷ.pdf", "", "", "g", "s", false, 2},
		{"26", "Octo方案v1.2.pdf", "", "", "g", "s", false, 2},
		{"27", "Octo方案v1.2.pdf", "", "v12", "g", "s", false, 2},
	}
	bulk := client.Bulk().Index(index).Refresh("true")
	for i, f := range fixtures {
		// Projection format is checked by the indexer's serialization tests.
		doc := map[string]any{"messageId": i + 1, "messageSeq": i + 1, "timestamp": 100, "subSeq": 0, "channelId": f.channel, "channelType": f.channelType, "spaceId": f.space, "revoked": f.revoked, "from": "sender", "payload": map[string]any{"type": 8, "file": map[string]any{"name": f.name, "nameLiteral": foldFilename(f.name), "caption": f.caption, "content": f.body}}}
		bulk.Add(elastic.NewBulkIndexRequest().Id(f.id).Doc(doc))
	}
	resp, err := bulk.Do(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Errors {
		t.Fatalf("index fixtures: %+v", resp.Failed())
	}
	analyzer := newOSIKSmartAnalyzer(client)
	for _, tc := range []struct {
		keyword string
		ids     []string
	}{
		{"octo", []string{"1", "2", "9", "12", "13", "26", "27"}},
		{"cto", []string{"1", "2", "9", "13", "26", "27"}},
		{"  Ｖ１．２  ", []string{"1", "9", "26", "27"}},
		{"v12", []string{"2", "27"}},
		{"排期 评审", []string{"3", "4", "5", "6", "16"}},
		{"*?", []string{"7"}},
		{"%_", []string{"7"}},
		{"STRASSE", []string{"8"}},
		{"カタカナ", []string{"17"}},
		{"ガイド", []string{"20", "21"}},
		{"ｶﾞｲﾄﾞ", []string{"20", "21"}},
		{"パーツ", []string{"22", "23"}},
		{"ﾊﾟｰﾂ", []string{"22", "23"}},
		{"ᾷ", []string{"24", "25"}},
		{"ᾷ", []string{"24", "25"}},
		{"繁体", nil},
		{"①", []string{"18"}},
		{"😀", []string{"8", "19"}},
	} {
		t.Run(tc.keyword, func(t *testing.T) {
			for _, entry := range []string{"channel-files", "global-files", "channel-all", "global-messages"} {
				t.Run(entry, func(t *testing.T) {
					var q elastic.Query
					var err error
					switch entry {
					case "channel-files":
						q, err = buildSearchFilesDSL(ctx, analyzer, true, SearchFilesReq{ChannelID: "g", ChannelType: 2, Keyword: tc.keyword}, "g", "s")
					case "global-files":
						q, err = buildGlobalFilesDSL(ctx, analyzer, true, SearchGlobalFilesReq{Keyword: tc.keyword}, []string{"g"}, "s")
					case "channel-all":
						q, err = buildSearchAllDSL(ctx, analyzer, true, SearchAllReq{ChannelID: "g", ChannelType: 2, Keyword: tc.keyword}, "g", "s")
					case "global-messages":
						q, err = buildGlobalMessagesDSL(ctx, analyzer, true, SearchGlobalMessagesReq{Keyword: tc.keyword}, []string{"g"}, "s")
					}
					if err != nil {
						t.Fatal(err)
					}
					var ids []string
					var after []any
					for {
						svc := client.Search(index).RestTotalHitsAsInt(true).Query(q).Sort("timestamp", false).Sort("messageId", false).Sort("subSeq", false).Size(2)
						if len(after) > 0 {
							svc = svc.SearchAfter(after...)
						}
						res, err := svc.Do(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if len(res.Hits.Hits) == 0 {
							break
						}
						for _, hit := range res.Hits.Hits {
							ids = append(ids, hit.Id)
						}
						after = res.Hits.Hits[len(res.Hits.Hits)-1].Sort
					}
					sort.Strings(ids)
					want := append([]string(nil), tc.ids...)
					sort.Strings(want)
					if !reflect.DeepEqual(ids, want) {
						t.Fatalf("IDs=%v want=%v", ids, want)
					}
				})
			}
		})
	}
	// Global DM double-guard still excludes an otherwise matching foreign Space.
	q, err := buildGlobalFilesDSL(ctx, analyzer, true, SearchGlobalFilesReq{Keyword: "octo"}, []string{"dm"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Search(index).RestTotalHitsAsInt(true).Query(q).Do(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits.Hits) != 1 || res.Hits.Hits[0].Id != "15" {
		t.Fatalf("DM Space filter changed: %+v", res.Hits.Hits)
	}
	// Compare the complete legacy population, not just the intersection. Scope
	// filters are fixed; every fixture here is a file, so the mixed query's
	// non-file fields cannot contribute a score.
	for _, keyword := range []string{"octo", "排期 评审", "排期的评审", "v1.2", "v12", "pdf"} {
		for _, strip := range []bool{false, true} {
			effective, useMSM := keyword, true
			if strip {
				effective, useMSM, err = AnalyzeKeyword(ctx, analyzer, keyword)
				if err != nil {
					t.Fatal(err)
				}
			}
			oldClause := buildKeywordClauseFromAnalyzed(effective, useMSM, "payload.file.name^2", "payload.file.caption", "payload.file.content")
			oldQuery := elastic.NewBoolQuery().Filter(elastic.NewTermQuery("channelId", "g"), elastic.NewTermQuery("payload.type", 8)).MustNot(elastic.NewTermQuery("revoked", true)).Must(oldClause)
			oldHits, err := client.Search(index).RestTotalHitsAsInt(true).Query(oldQuery).Size(100).Do(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(oldHits.Hits.Hits) == 0 && keyword != "v1.2" {
				t.Fatalf("empty legacy baseline for %q strip=%v", keyword, strip)
			}
			for _, entry := range []string{"channel-files", "global-files", "channel-all", "global-messages"} {
				q, err := filenameEntryQuery(ctx, analyzer, strip, entry, keyword)
				if err != nil {
					t.Fatal(err)
				}
				newHits, err := client.Search(index).RestTotalHitsAsInt(true).Query(q).Size(100).Do(ctx)
				if err != nil {
					t.Fatal(err)
				}
				scores := map[string]float64{}
				for _, hit := range newHits.Hits.Hits {
					scores[hit.Id] = *hit.Score
				}
				for _, hit := range oldHits.Hits.Hits {
					score, exists := scores[hit.Id]
					if !exists {
						t.Fatalf("%s %q strip=%v lost legacy ID %s", entry, keyword, strip, hit.Id)
					}
					if math.Abs(score-*hit.Score) > 1e-6 {
						t.Fatalf("%s %q score changed for %s: %v -> %v", entry, keyword, hit.Id, *hit.Score, score)
					}
				}
				// The grouped route aggregates the same global-message base query.
				if entry == "global-messages" {
					grouped, err := client.Search(index).RestTotalHitsAsInt(true).Query(q).Size(0).Aggregation("channels", elastic.NewTermsAggregation().Field("channelId")).Do(ctx)
					if err != nil {
						t.Fatal(err)
					}
					buckets, ok := grouped.Aggregations.Terms("channels")
					if !ok || len(buckets.Buckets) != 1 || int(buckets.Buckets[0].DocCount) != len(newHits.Hits.Hits) {
						t.Fatalf("%q grouped count does not match complete restored feed", keyword)
					}
				}
			}
		}
	}
}

func filenameEntryQuery(ctx context.Context, analyzer tokenAnalyzer, strip bool, entry, keyword string) (elastic.Query, error) {
	switch entry {
	case "channel-files":
		return buildSearchFilesDSL(ctx, analyzer, strip, SearchFilesReq{ChannelID: "g", ChannelType: 2, Keyword: keyword}, "g", "s")
	case "global-files":
		return buildGlobalFilesDSL(ctx, analyzer, strip, SearchGlobalFilesReq{Keyword: keyword}, []string{"g"}, "s")
	case "channel-all":
		return buildSearchAllDSL(ctx, analyzer, strip, SearchAllReq{ChannelID: "g", ChannelType: 2, Keyword: keyword}, "g", "s")
	case "global-messages":
		return buildGlobalMessagesDSL(ctx, analyzer, strip, SearchGlobalMessagesReq{Keyword: keyword}, []string{"g"}, "s")
	default:
		return nil, fmt.Errorf("unknown filename entry %q", entry)
	}
}
