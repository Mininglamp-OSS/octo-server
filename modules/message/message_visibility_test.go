package message

import (
	"testing"
	"time"
)

func TestMessageVisibleToViewer(t *testing.T) {
	const uid = "u1"
	now := time.Now().Unix()
	textPayload := []byte(`{"type":1,"content":"hi"}`)

	baseMsg := func() *messageModel {
		return &messageModel{MessageID: 1, MessageSeq: 5, Payload: textPayload, Timestamp: now}
	}

	cases := []struct {
		name             string
		msg              *messageModel
		extra            *messageExtraDetailModel
		userExtra        *messageUserExtraModel
		userOffsetSeq    uint32
		channelOffsetSeq uint32
		want             bool
	}{
		{"visible_baseline", baseMsg(), nil, nil, 0, 0, true},
		{"visibles_excludes", &messageModel{MessageSeq: 5, Payload: []byte(`{"type":1,"visibles":["other"]}`), Timestamp: now}, nil, nil, 0, 0, false},
		{"visibles_includes", &messageModel{MessageSeq: 5, Payload: []byte(`{"type":1,"visibles":["u1"]}`), Timestamp: now}, nil, nil, 0, 0, true},
		{"revoked", baseMsg(), &messageExtraDetailModel{messageExtraModel: messageExtraModel{Revoke: 1}}, nil, 0, 0, false},
		{"global_deleted", baseMsg(), &messageExtraDetailModel{messageExtraModel: messageExtraModel{IsDeleted: 1}}, nil, 0, 0, false},
		{"user_deleted", baseMsg(), nil, &messageUserExtraModel{MessageIsDeleted: 1}, 0, 0, false},
		{"expired", &messageModel{MessageSeq: 5, Payload: textPayload, Expire: 10, Timestamp: now - 100}, nil, nil, 0, 0, false},
		{"not_expired", &messageModel{MessageSeq: 5, Payload: textPayload, Expire: 10, Timestamp: now}, nil, nil, 0, 0, true},
		{"user_offset_truncated", baseMsg(), nil, nil, 5, 0, false},    // seq(5) <= userOffset(5)
		{"user_offset_below", baseMsg(), nil, nil, 4, 0, true},         // seq(5) > userOffset(4)
		{"channel_offset_truncated", baseMsg(), nil, nil, 0, 5, false}, // seq(5) <= channelOffset(5)
		{"channel_offset_below", baseMsg(), nil, nil, 0, 4, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageVisibleToViewer(tc.msg, tc.extra, tc.userExtra, tc.userOffsetSeq, tc.channelOffsetSeq, uid); got != tc.want {
				t.Fatalf("messageVisibleToViewer = %v, want %v", got, tc.want)
			}
		})
	}
}
