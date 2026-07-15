package bot_api

import (
	"os"
	"strings"
	"testing"
)

func TestCardSeqEditDelegatesRetryPolicyToSharedMutator(t *testing.T) {
	raw, err := os.ReadFile("send.go")
	if err != nil {
		t.Fatalf("read send.go: %v", err)
	}
	source := string(raw)
	start := strings.Index(source, "if hasCardSeq {")
	if start < 0 {
		t.Fatal("send.go is missing the card_seq edit branch")
	}
	end := strings.Index(source[start:], "\n\t} else {")
	if end < 0 {
		t.Fatal("send.go card_seq edit branch has no boundary")
	}
	branch := source[start : start+end]
	if strings.Contains(branch, "for attempt :=") {
		t.Fatal("card_seq edit branch must not wrap the shared CAS mutator in another retry loop")
	}
	if strings.Count(branch, "ba.cardSeqCASWrite(") != 1 {
		t.Fatalf("card_seq edit branch must call cardSeqCASWrite exactly once:\n%s", branch)
	}
}
