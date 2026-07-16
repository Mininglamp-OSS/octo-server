package main

// Standalone tool that writes the three sample cards' AC JSON to stdout as
// `{"summary_completed": {...}, "summary_failed": {...}, "docs_shared": {...}}`.
// Uses the same pkg/cardtmpl + modules/notify code paths the production
// producers run.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

func main() {
	ctx := context.Background()
	base := "https://im.example.com/login"

	summaryCompleted := mustCard(cardtmpl.BuildSummaryResourceCard(
		ctx, base, "TN_20260713_abcd", "spc_xxx",
		cardtmpl.ResourceCard{
			Title:       "产品周会纪要",
			Attribution: "总结已生成完成",
			Facts: []cardtmpl.Fact{
				{Title: "时间范围", Value: "2026-07-06 10:00 ~ 2026-07-13 10:00"},
				{Title: "参与成员", Value: "5 人"},
				{Title: "消息数量", Value: "128 条"},
				{Title: "生成时间", Value: "2026-07-13 15:04"},
			},
			Variant: "summary.completed",
			Source:  cardtmpl.Source{Label: "智能总结"},
		},
	))

	summaryFailed := mustCard(cardtmpl.BuildSummaryResourceCard(
		ctx, base, "TN_20260713_abcd", "spc_xxx",
		cardtmpl.ResourceCard{
			Title:       "产品周会纪要",
			Attribution: "总结生成失败",
			Excerpt:     "失败原因：upstream LLM 5xx",
			Variant:     "summary.failed",
			Source:      cardtmpl.Source{Label: "智能总结"},
		},
	))

	docsShared := mustCard(cardtmpl.BuildDocsResourceCard(
		ctx, base, "d_20260713_abcd", "spc_xxx",
		cardtmpl.ResourceCard{
			Title:       "产品设计方案",
			Attribution: "Alice 分享了文档",
			Excerpt:     "Q3 上线计划已确认",
			Facts: []cardtmpl.Fact{
				{Title: "操作人", Value: "Alice"},
				{Title: "时间", Value: "2026-07-13 15:04"},
			},
			Variant: "docs.shared",
			Source:  cardtmpl.Source{Label: "文档"},
		},
	))

	out := map[string]json.RawMessage{
		"summary_completed": summaryCompleted,
		"summary_failed":    summaryFailed,
		"docs_shared":       docsShared,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	_, _ = os.Stdout.Write(b)
}

func mustCard(raw json.RawMessage, err error) json.RawMessage {
	if err != nil {
		fatal("build card: %v", err)
	}
	return raw
}

func fatal(f string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}
