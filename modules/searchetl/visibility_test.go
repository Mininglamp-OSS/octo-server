package searchetl

import (
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/contract/searchmsg"
)

// TestExtractMessage_SharedFailClosedVectors 跑 octo-lib 共享的 fail-closed 可见性向量集
// （验收门 (i)+(ii)）：producer 与 octo-search-indexer backfill 跑**同一组**向量锁口径。
//
// 安全口径（ReviewBot YUJ-4953 钉死）：非加密群消息 visibles
//   - 解析失败 → outcomeDLQ（绝不进 main topic）
//   - valid-but-empty（null / [] / 全非字符串）→ 同样 outcomeDLQ
// visibles 键缺失 = 广播消息 → outcomeOK 进 main，visibles 为空（reader 无 gate 属预期安全）。
func TestExtractMessage_SharedFailClosedVectors(t *testing.T) {
	for _, v := range searchmsg.FailClosedVisibilityVectors() {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			// 非加密 type=Text 行：直接用向量 payload（向量本身已含 type/content）。
			row := &srcMessageRow{MessageID: "m_" + v.Name, ChannelType: common.ChannelTypeGroup.Uint8(), Payload: v.Payload}
			msg, outcome := extractMessage(row)

			if v.WantErr {
				// 🔴 非加密、可见性不可信 → 必须落 DLQ，绝不进 main topic。
				if outcome != outcomeDLQ {
					t.Fatalf("%s: visibility fail-closed must route to DLQ, got outcome=%v visibles=%v", v.Name, outcome, msg.Visibles)
				}
				// DLQ 消息绝不能携带空 visibles 假装广播进主流（即便它最终不投 main，也防误用）。
				if len(msg.Visibles) != 0 {
					t.Fatalf("%s: DLQ msg must not carry visibles, got %v", v.Name, msg.Visibles)
				}
				return
			}

			// 放行类：必须不是 DLQ，且 visibles/spaceID 与期望一致。
			if outcome == outcomeDLQ {
				t.Fatalf("%s: must not DLQ, got outcome=%v", v.Name, outcome)
			}
			if msg.SpaceID != v.WantSpaceID {
				t.Fatalf("%s: spaceID=%q want %q", v.Name, msg.SpaceID, v.WantSpaceID)
			}
			if !reflect.DeepEqual(msg.Visibles, v.WantVisibles) {
				t.Fatalf("%s: visibles=%v want %v", v.Name, msg.Visibles, v.WantVisibles)
			}
		})
	}
}

// TestExtractMessage_EmptyVisiblesGroupMsgDLQ 单独钉死验收门 (i) 的核心：
// 非加密群消息 valid-but-empty visibles → DLQ 不进 main（ReviewBot 特别强调 population 主落点）。
func TestExtractMessage_EmptyVisiblesGroupMsgDLQ(t *testing.T) {
	row := &srcMessageRow{
		MessageID:   "grp-empty-vis",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":99,"content":"you were removed","visibles":[]}`),
	}
	_, outcome := extractMessage(row)
	if outcome != outcomeDLQ {
		t.Fatalf("group msg with valid-but-empty visibles MUST DLQ (else reader fail-OPEN #1124), got %v", outcome)
	}
}

// TestExtractMessage_UnparseableVisiblesGroupMsgDLQ 非加密群消息 visibles 解析失败 → DLQ。
func TestExtractMessage_UnparseableVisiblesGroupMsgDLQ(t *testing.T) {
	row := &srcMessageRow{
		MessageID:   "grp-bad-vis",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":99,"content":"x","visibles":"not-an-array"}`),
	}
	_, outcome := extractMessage(row)
	if outcome != outcomeDLQ {
		t.Fatalf("group msg with unparseable visibles MUST DLQ, got %v", outcome)
	}
}

// TestExtractMessage_ValidVisiblesEnriched 合法定向系统消息 → 进 main 且 visibles 富化进契约。
func TestExtractMessage_ValidVisiblesEnriched(t *testing.T) {
	row := &srcMessageRow{
		MessageID:   "grp-ok-vis",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":99,"content":"removed","visibles":["u_alice","u_bob"]}`),
	}
	msg, outcome := extractMessage(row)
	if outcome == outcomeDLQ {
		t.Fatalf("valid targeted system msg must not DLQ, got %v", outcome)
	}
	if !reflect.DeepEqual(msg.Visibles, []string{"u_alice", "u_bob"}) {
		t.Fatalf("visibles must be enriched into contract, got %v", msg.Visibles)
	}
}

// TestExtractMessage_NormalGroupChatBroadcast 普通群聊正文（无 visibles 键）→ 进 main 广播，
// visibles 为空属预期（否则全部正常消息被灌进 DLQ）。
func TestExtractMessage_NormalGroupChatBroadcast(t *testing.T) {
	row := &srcMessageRow{
		MessageID:   "grp-chat",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":1,"content":"hello everyone"}`),
	}
	msg, outcome := extractMessage(row)
	if outcome != outcomeOK {
		t.Fatalf("normal group chat must be outcomeOK (broadcast), got %v", outcome)
	}
	if len(msg.Visibles) != 0 {
		t.Fatalf("broadcast msg must have empty visibles, got %v", msg.Visibles)
	}
}

// TestExtractMessage_MessageSeqEnriched messageSeq 从 message.message_seq 列取并填进契约。
func TestExtractMessage_MessageSeqEnriched(t *testing.T) {
	row := &srcMessageRow{
		MessageID:   "seq-row",
		MessageSeq:  4242,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":1,"content":"x"}`),
	}
	msg, outcome := extractMessage(row)
	if outcome != outcomeOK {
		t.Fatalf("outcome=%v", outcome)
	}
	if msg.MessageSeq != 4242 {
		t.Fatalf("messageSeq must be carried from row, got %d", msg.MessageSeq)
	}
}

// TestExtractMessage_MessageSeqFullWidth message_seq 是 BIGINT/uint64：超过 uint32 上限的
// 序号必须无损全精度透传，不得截断（高序号频道「清空会话」gate 正确性依赖此）。
func TestExtractMessage_MessageSeqFullWidth(t *testing.T) {
	const big = uint64(1) << 40 // 远超 math.MaxUint32
	row := &srcMessageRow{
		MessageID:   "seq-big",
		MessageSeq:  big,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":1,"content":"x"}`),
	}
	msg, _ := extractMessage(row)
	if msg.MessageSeq != big {
		t.Fatalf("messageSeq must be carried full-width without truncation, got %d want %d", msg.MessageSeq, big)
	}
}

// TestExtractMessage_EncryptedSkipsVisibility 加密 DM：payload 是密文不解析，
// spaceID/visibles 留空走 reader fail-closed，且不因密文非 JSON 而误 DLQ。
func TestExtractMessage_EncryptedSkipsVisibility(t *testing.T) {
	row := &srcMessageRow{
		MessageID:   "enc",
		Signal:      1,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte("ENCRYPTED-CIPHERTEXT-NOT-JSON"),
	}
	msg, outcome := extractMessage(row)
	if outcome != outcomeRawExcluded {
		t.Fatalf("encrypted msg must be raw_excluded, got %v", outcome)
	}
	if msg.SpaceID != "" || len(msg.Visibles) != 0 {
		t.Fatalf("encrypted msg must leave spaceID/visibles empty (fail-closed), got %q %v", msg.SpaceID, msg.Visibles)
	}
}
