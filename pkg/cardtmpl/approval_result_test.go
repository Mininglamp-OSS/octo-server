package cardtmpl

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildApprovalResultCardOwnsShapeAndEscapesDisplayText(t *testing.T) {
	raw, err := BuildApprovalResultCard(ApprovalResultCard{
		Title: "**Deploy**", Status: "Request approved", Variant: "approval.approved", Source: "Approval",
	})
	if err != nil {
		t.Fatalf("BuildApprovalResultCard() error = %v", err)
	}
	var card map[string]interface{}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if _, ok := card["actions"]; ok {
		t.Fatal("approval result card must not contain actions")
	}
	metadata, _ := card["metadata"].(map[string]interface{})
	if _, ok := metadata["webUrl"]; ok {
		t.Fatal("generic approval result must not trust a consumer URL")
	}
	if !strings.Contains(string(raw), `\\*\\*Deploy\\*\\*`) {
		t.Fatalf("display title was not markdown escaped: %s", raw)
	}
}

func TestBuildApprovalResultCardTruncatesTitleAndRejectsMissingStatus(t *testing.T) {
	raw, err := BuildApprovalResultCard(ApprovalResultCard{Title: strings.Repeat("界", maxTitleRunes+10), Status: "ok"})
	if err != nil {
		t.Fatalf("BuildApprovalResultCard(long title) error = %v", err)
	}
	var card struct {
		Body []struct {
			Text string `json:"text"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if len(card.Body) == 0 || utf8.RuneCountInString(card.Body[0].Text) != maxTitleRunes {
		t.Fatalf("title rune count = %d, want %d", utf8.RuneCountInString(card.Body[0].Text), maxTitleRunes)
	}
	if _, err := BuildApprovalResultCard(ApprovalResultCard{Title: "x"}); err == nil {
		t.Fatal("BuildApprovalResultCard(missing status) error = nil")
	}
}
