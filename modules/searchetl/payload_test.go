package searchetl

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
)

// textPayload 构造一条 type=Text 的明文 payload。
func textPayload(t *testing.T, content string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{"type": common.Text.Int(), "content": content})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// signalSetting 返回带 Signal 位的 setting uint8。
func signalSetting() uint8 {
	return config.Setting{Signal: true}.ToUint8()
}

// TestExtract_Text 正常文本 → outcomeOK，content 取出，非 raw_excluded。
func TestExtract_Text(t *testing.T) {
	row := &srcMessageRow{MessageID: "m1", ChannelType: 2, Payload: textPayload(t, "hello 世界")}
	msg, outcome := extractMessage(row)
	if outcome != outcomeOK {
		t.Fatalf("want outcomeOK, got %v", outcome)
	}
	if msg.RawExcluded {
		t.Fatalf("text msg must not be raw_excluded")
	}
	if msg.Content == nil || *msg.Content != "hello 世界" {
		t.Fatalf("content mismatch: %+v", msg.Content)
	}
	if msg.ContentType != common.Text.Int() {
		t.Fatalf("content_type=%d", msg.ContentType)
	}
}

// TestExtract_SignalViaSetting Signal 加密（setting 位）→ raw_excluded，content=nil，不进 DLQ。
func TestExtract_SignalViaSetting(t *testing.T) {
	row := &srcMessageRow{MessageID: "m2", Setting: signalSetting(), Payload: []byte("ENCRYPTED-NOT-JSON")}
	msg, outcome := extractMessage(row)
	if outcome != outcomeRawExcluded {
		t.Fatalf("want outcomeRawExcluded, got %v", outcome)
	}
	if !msg.RawExcluded || msg.Content != nil {
		t.Fatalf("signal msg must be raw_excluded with nil content: %+v", msg)
	}
}

// TestExtract_SignalViaColumn Signal 加密（signal 列）→ raw_excluded，即便 payload 恰为 JSON 也不解析。
func TestExtract_SignalViaColumn(t *testing.T) {
	row := &srcMessageRow{MessageID: "m3", Signal: 1, Payload: textPayload(t, "should be ignored")}
	msg, outcome := extractMessage(row)
	if outcome != outcomeRawExcluded {
		t.Fatalf("want outcomeRawExcluded, got %v", outcome)
	}
	if !msg.RawExcluded || msg.Content != nil {
		t.Fatalf("signal-column msg must be raw_excluded: %+v", msg)
	}
}

// TestExtract_NonTextRawExcluded 非文本（媒体）→ raw_excluded，不进 DLQ。
func TestExtract_NonTextRawExcluded(t *testing.T) {
	b, _ := json.Marshal(map[string]interface{}{"type": common.Image.Int(), "url": "http://x/y.png"})
	row := &srcMessageRow{MessageID: "m4", Payload: b}
	msg, outcome := extractMessage(row)
	if outcome != outcomeRawExcluded {
		t.Fatalf("want outcomeRawExcluded for non-text, got %v", outcome)
	}
	if !msg.RawExcluded || msg.Content != nil {
		t.Fatalf("non-text must be raw_excluded: %+v", msg)
	}
}

// TestExtract_TextContentObjectRawExcluded type=Text 但 content 为 object（bot 误塞）→ 保守 raw_excluded。
func TestExtract_TextContentObjectRawExcluded(t *testing.T) {
	b, _ := json.Marshal(map[string]interface{}{"type": common.Text.Int(), "content": map[string]interface{}{"k": "v"}})
	row := &srcMessageRow{MessageID: "m5", Payload: b}
	msg, outcome := extractMessage(row)
	if outcome != outcomeRawExcluded {
		t.Fatalf("want outcomeRawExcluded for text/object content, got %v", outcome)
	}
	if msg.Content != nil {
		t.Fatalf("content must be nil: %+v", msg.Content)
	}
}

// TestExtract_NonSignalBadJSON 非加密但 payload 非法 JSON（真异常）→ outcomeDLQ。
func TestExtract_NonSignalBadJSON(t *testing.T) {
	row := &srcMessageRow{MessageID: "m6", Payload: []byte("{not valid json")}
	msg, outcome := extractMessage(row)
	if outcome != outcomeDLQ {
		t.Fatalf("want outcomeDLQ for bad json, got %v", outcome)
	}
	if msg.MessageID != "m6" {
		t.Fatalf("dlq msg must keep message_id for triage")
	}
}

// TestExtract_TypeAsFloat 兼容 json 反序列化把 type 解成 float64。
func TestExtract_TypeAsFloat(t *testing.T) {
	// 直接 json.Unmarshal 的结果 type 即 float64。
	row := &srcMessageRow{MessageID: "m7", Payload: []byte(`{"type":1,"content":"x"}`)}
	msg, outcome := extractMessage(row)
	if outcome != outcomeOK || msg.Content == nil || *msg.Content != "x" {
		t.Fatalf("float64 type Text not handled: outcome=%v content=%v", outcome, msg.Content)
	}
}
