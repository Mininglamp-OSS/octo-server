package namesearch

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestS14GoldenFixture(t *testing.T) {
	var fixture struct {
		Version string
		Scores  []struct {
			Name, Query string
			Score       int
		}
		Rankings []struct {
			Query       string
			Candidates  []Candidate
			ExpectedIDs []string `json:"expected_ids"`
		}
	}
	b, err := os.ReadFile("testdata/s14-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != RulesVersion || len(fixture.Scores) != 9 || len(fixture.Rankings) != 2 {
		t.Fatal("incomplete S14 fixture")
	}
	for _, tc := range fixture.Scores {
		m, err := MatchField(tc.Query, tc.Name, false)
		if err != nil || m == nil || m.Score != tc.Score {
			t.Fatalf("%s: %+v %v", tc.Query, m, err)
		}
	}
	for _, tc := range fixture.Rankings {
		hits, err := RankCandidates(tc.Query, tc.Candidates)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, h := range hits {
			ids = append(ids, h.Candidate.ID)
		}
		if !reflect.DeepEqual(ids, tc.ExpectedIDs) {
			t.Fatalf("%s: %v", tc.Query, ids)
		}
	}
}
