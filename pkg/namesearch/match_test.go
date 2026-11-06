package namesearch

import (
	"reflect"
	"testing"
)

func TestNineScores(t *testing.T) {
	for _, tc := range []struct {
		query string
		score int
	}{
		{"向晨瑜", 1000}, {"向", 900}, {"瑜", 800}, {"xiangchenyu", 700},
		{"xiangchen", 650}, {"chenyu", 600}, {"xcy", 500}, {"xc", 450}, {"cy", 400},
	} {
		t.Run(tc.query, func(t *testing.T) {
			m, err := MatchField(tc.query, "向晨瑜", false)
			if err != nil || m == nil || m.Score != tc.score {
				t.Fatalf("got %+v, %v; want %d", m, err, tc.score)
			}
		})
	}
}

func TestNameMatching(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		match       bool
	}{
		{"向晨瑜", "xiangc", true}, {"向晨瑜", "xiangcy", true}, {"向晨瑜", "向cy", true}, {"向晨瑜", "向cheny", true},
		{"向晨瑜", "xy", false}, {"向晨瑜", "ychen", false},
		{"向 晨瑜", "xiangchenyu", true}, {"向.晨-瑜AI", "xiangchenyuai", true},
		{"向晨瑜", "向.c", false}, {"Octo", " ＯＣＴＯ ", true}, {"刘向东", "劉向東", true},
		{"A.B", "AB", false}, {"X%_Y", "%", true}, {"ABC", "%", false}, {"ABC", "_", false},
		{"吕洋", "lvyang", true}, {"吕洋", "lüyang", true}, {"吕洋", "luyang", false},
		{"许可", "xuke", true}, {"许可", "xvke", true}, {"单明", "shanming", true}, {"单明", "danming", false},
		{"曾华", "zenghua", true}, {"重庆", "chongqing", true}, {"长沙", "changsha", true},
		{"向晨瑜", "xiàng", false}, {"", "x", false}, {"向晨瑜", "   ", false},
		{"向XV", "xiangxv", true}, {"向XV", "xiangxu", false},
		{"许可AI", "xvkeai", true}, {"许可", "许可xv", false},
		{"X向晨瑜", "x", true}, {"123", "2", true}, {"A\\B", "\\", true}, {"A'B", "'", true},
	} {
		t.Run(tc.name+"/"+tc.query, func(t *testing.T) {
			m, err := MatchField(tc.query, tc.name, false)
			if err != nil || (m != nil) != tc.match {
				t.Fatalf("got %+v %v; match=%v", m, err, tc.match)
			}
		})
	}
}

func TestRangesAndRemarks(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		ranges      []Range
		start       int
	}{
		{"向 晨瑜", "xcy", []Range{{0, 1}, {2, 4}}, 0},
		{"小王和小王", "小王", []Range{{0, 2}, {3, 5}}, 0},
		{"😀刘向东", "向", []Range{{2, 3}}, 2},
		{"劉向東", "刘向东", []Range{{0, 3}}, 0},
		{"向晨瑜", "iangc", []Range{{0, 2}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := MatchField(tc.query, tc.name, false)
			if err != nil || m == nil || !reflect.DeepEqual(m.Ranges, tc.ranges) || m.Start != tc.start {
				t.Fatalf("got %+v %v; want %v", m, err, tc.ranges)
			}
		})
	}
	hits, err := RankCandidates("向", []Candidate{{ID: "1", Kind: "contact", ContactType: "human", OriginalName: "向丁", RemarkName: "刘向"}})
	if err != nil || len(hits) != 1 || hits[0].Best.Score != 900 || hits[0].Remark.Score != 820 || hits[0].Original.Score != 900 {
		t.Fatalf("got %+v %v", hits, err)
	}
	hits, err = RankCandidates("小王", []Candidate{{ID: "1", Kind: "contact", ContactType: "human", OriginalName: "小王", RemarkName: "小王"}})
	if err != nil || len(hits) != 1 || hits[0].Best.Score != 1020 {
		t.Fatalf("got %+v %v", hits, err)
	}
	hits, err = RankCandidates("王明产品", []Candidate{{ID: "1", Kind: "contact", ContactType: "human", OriginalName: "王明", RemarkName: "产品负责人"}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("fields must not concatenate: %+v %v", hits, err)
	}
}

func TestPRDOrdering(t *testing.T) {
	people := []Candidate{
		{ID: "7", OriginalName: "王丁", RemarkName: "同事小王"},
		{ID: "5", OriginalName: "王丙", RemarkName: "小王同事"},
		{ID: "4", OriginalName: "小王项目", RemarkName: "小王伙伴"},
		{ID: "6", OriginalName: "小王团队", RemarkName: "老王"},
		{ID: "3", OriginalName: "小王"}, {ID: "2", OriginalName: "王乙", RemarkName: "小王"}, {ID: "1", OriginalName: "王甲", RemarkName: "小王"},
	}
	for i := range people {
		people[i].Kind = "contact"
		people[i].ContactType = "human"
	}
	hits, err := RankCandidates("小王", people)
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.Candidate.ID)
	}
	if err != nil || !reflect.DeepEqual(ids, []string{"1", "2", "3", "4", "5", "6", "7"}) {
		t.Fatalf("got %v %v", ids, err)
	}
	names := []string{"向东", "刘向东", "向导虾", "向日葵", "向日葵AI", "向日葵女士", "向日葵vip", "万向轮（数据穿透）需求管家", "要天天向上的巡检员", "social向量查询"}
	people = nil
	for i := len(names) - 1; i >= 0; i-- {
		typ := "ai"
		if i < 2 {
			typ = "human"
		}
		people = append(people, Candidate{ID: names[i], Kind: "contact", ContactType: typ, OriginalName: names[i]})
	}
	hits, err = RankCandidates("向", people)
	got := []string{}
	for _, h := range hits {
		got = append(got, h.Candidate.OriginalName)
	}
	if err != nil || !reflect.DeepEqual(got, names) {
		t.Fatalf("got %v %v", got, err)
	}
	people = []Candidate{{ID: "2", Kind: "contact", ContactType: "ai", RemarkName: "向"}, {ID: "10", Kind: "contact", ContactType: "human", OriginalName: "刘向东"}}
	hits, err = RankCandidates("向", people)
	if err != nil || len(hits) != 2 || hits[0].Candidate.ID != "10" {
		t.Fatalf("human weak match must win: %+v %v", hits, err)
	}
	people = []Candidate{{ID: "2", Kind: "group", OriginalName: "向"}, {ID: "10", Kind: "group", OriginalName: "向"}}
	hits, err = RankCandidates("向", people)
	if err != nil || len(hits) != 2 || hits[0].Candidate.ID != "10" {
		t.Fatalf("ID codepoint tie: %+v %v", hits, err)
	}
}

func TestMixedAndInitialMatchesCoexist(t *testing.T) {
	for _, tc := range []struct {
		name         string
		score, start int
		ranges       []Range
	}{
		{"向晨瑜xiangcy", 800, 3, []Range{{0, 10}}},
		{"向晨瑜许I安宁高成玉", 650, 0, []Range{{0, 10}}},
	} {
		m, err := MatchField("xiangcy", tc.name, false)
		if err != nil || m == nil || m.Score != tc.score || m.Start != tc.start || !reflect.DeepEqual(m.Ranges, tc.ranges) {
			t.Fatalf("%s: got %+v %v", tc.name, m, err)
		}
	}
}
