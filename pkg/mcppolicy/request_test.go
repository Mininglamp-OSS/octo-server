package mcppolicy

import (
	"strings"
	"testing"
)

func TestParseMutation(t *testing.T) {
	for _, tc := range []struct {
		body       string
		del, valid bool
	}{
		{`{"context_mode":"compatible","if_generation":"18446744073709551615"}`, false, true},
		{`{"if_generation":"2"}`, true, true},
		{`{"context_mode":"strict","if_generation":"7"}`, false, true},
		{`{"context_mode":"strict","if_generation":7}`, false, false},
		{`{"context_mode":"Strict","if_generation":"7"}`, false, false},
		{`{"context_mode":"strict ","if_generation":"7"}`, false, false},
		{`{"Context_mode":"strict","if_generation":"7"}`, false, false},
		{`{"context_mode":null,"if_generation":"7"}`, false, false},
		{`{"context_mode":"strict","if_generation":null}`, false, false},
		{`{"context_mode":"strict","if_generation":"07"}`, false, false},
		{`{"context_mode":"strict","if_generation":"0"}`, false, false},
		{`{"context_mode":"strict","if_generation":"+7"}`, false, false},
		{`{"context_mode":"strict","if_generation":"18446744073709551616"}`, false, false},
		{`{"context_mode":"strict","if_generation":"7","if_\u0067eneration":"8"}`, false, false},
		{`{"context_mode":"strict","if_generation":"7","extra":true}`, false, false},
		{`{"context_mode":"strict","if_generation":"7"}`, true, false},
		{`{"if_generation":"1"} {}`, true, false},
		{`{"if_generation":"1"} garbage`, true, false},
		{`{}`, true, false}, {`null`, true, false}, {``, true, false},
		{`{"if_generation":"1"}` + strings.Repeat(" ", 2048), true, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			_, err := ParseMutation(strings.NewReader(tc.body), tc.del)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestVerifyInclude(t *testing.T) {
	for _, tc := range []struct {
		body           string
		include, valid bool
	}{
		{`{"bot_token":"synthetic"}`, false, true},
		{`{"bot_token":"synthetic","include_mcp_policy":false,"future":{}}`, false, true},
		{`{"include_mcp_policy":true}`, true, true},
		{`{"include_mcp_policy":null}`, false, false},
		{`{"include_mcp_policy":"true"}`, false, false},
		{`{"include_mcp_policy":1}`, false, false},
		{`{"include_mcp_policy":false,"include_mcp_polic\u0079":true}`, false, false},
		{`{"Include_mcp_policy":true}`, false, false},
		{`null`, false, false}, {`[]`, false, false},
	} {
		got, err := VerifyInclude([]byte(tc.body))
		if got != tc.include || (err == nil) != tc.valid {
			t.Fatalf("%s: %v, %v", tc.body, got, err)
		}
	}
}

func TestValidUID(t *testing.T) {
	for _, uid := range []string{"", " bot", "bot ", "bo\nt", "bo\x7ft", strings.Repeat("a", 41), strings.Repeat("猫", 14), "\xff"} {
		if ValidUID(uid) {
			t.Fatalf("accepted %q", uid)
		}
	}
	for _, uid := range []string{"bot_B", strings.Repeat("a", 40), strings.Repeat("猫", 13)} {
		if !ValidUID(uid) {
			t.Fatalf("rejected %q", uid)
		}
	}
}
