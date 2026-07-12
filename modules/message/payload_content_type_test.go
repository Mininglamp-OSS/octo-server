package message

import "testing"

func TestPayloadIsPlainText(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"text", `{"type":1,"content":"hello"}`, true},
		{"text_extra_fields", `{"type":1,"content":"hi","mention":{"all":true}}`, true},
		{"image", `{"type":2,"url":"http://x/a.png"}`, false},
		{"card", `{"type":7}`, false},
		{"system_tip", `{"type":2000}`, false},
		{"cmd", `{"type":99}`, false},
		{"missing_type", `{"content":"no type"}`, false},
		{"float_type", `{"type":1.0}`, false},
		{"string_int_text", `{"type":"1"}`, true},   // json.Number 接受带引号整数，语义等价 type=1
		{"string_int_image", `{"type":"2"}`, false}, // 带引号但非文本仍拒绝
		{"not_json", `not json`, false},
		{"empty", ``, false},
		{"null", `null`, false},
		{"array", `[1,2,3]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := payloadIsPlainText([]byte(tc.payload)); got != tc.want {
				t.Fatalf("payloadIsPlainText(%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}
