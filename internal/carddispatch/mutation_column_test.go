package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

// 一个合法但存不下的帧必须在写库前被确定性地拒掉。没有这道闸时，同一个帧会走到
// INSERT：严格模式下 MySQL 抛 `Data too long for column`（调用方看到 500），
// 非严格模式下静默截断成非法 JSON（客户端渲染不出的坏帧，且写成功了没人知道）。
// 两种都比一个带字节数的拒绝难查得多。
func TestCardMutatorRejectsFramesWiderThanTheColumn(t *testing.T) {
	original := testCardEnvelope(t, 0, true)

	// 一个 TextBlock 撑到超过列宽。schema 层面完全合法（cardmsg 只有 512 KiB 的
	// payload 上限，比列宽宽 8 倍），所以这个帧过得了 Validate、过不了列宽。
	oversized := func() string {
		card := map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{
				"type": "TextBlock", "text": strings.Repeat("塞", maxContentEditBytes/2),
			}},
		}
		envelope := map[string]interface{}{
			"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
			"profile": cardmsg.ProfileV2, "card": card, "space_id": "space-1", "card_seq": 42,
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}()

	// 先自证前提：这个帧是 cardmsg 眼里合法的，被拒的原因只能是列宽。
	if _, err := cardmsg.NormalizeContentEdit(oversized); err != nil {
		t.Fatalf("测试前提不成立：帧本身就非法 (%v)，那它就测不到列宽闸", err)
	}

	backend := &fakeMutationBackend{message: storedCardMessage{
		MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original,
	}}
	_, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
		SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
		ContentEdit: oversized,
	})
	if !errors.Is(err, ErrCardMutationTooLarge) {
		t.Fatalf("Mutate(oversized) error = %v, want ErrCardMutationTooLarge", err)
	}
	// wrap 关系必须保持：既有调用方（bot_api → ErrBotAPICardInvalid）只认这个哨兵。
	if !errors.Is(err, ErrCardMutationInvalid) {
		t.Fatalf("ErrCardMutationTooLarge 必须 wrap ErrCardMutationInvalid，否则既有错误映射失效；err = %v", err)
	}
	// 错误消息要带上字节数，否则运维只知道「太大」不知道大多少。
	if !strings.Contains(err.Error(), "65535") {
		t.Errorf("错误消息应含列宽以便定位：%v", err)
	}
	if len(backend.writes) != 0 || len(backend.revisions) != 0 || len(backend.syncs) != 0 {
		t.Fatalf("超宽帧不得触达任何写路径：writes=%d revisions=%d syncs=%d",
			len(backend.writes), len(backend.revisions), len(backend.syncs))
	}

	// 边界另一侧：刚好在列宽内的帧照常写入。闸不能把正常流量拦下来。
	backend = &fakeMutationBackend{message: storedCardMessage{
		MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original,
	}}
	result, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
		SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
		ContentEdit: string(testCardEnvelope(t, 42, false)),
	})
	if err != nil || !result.Applied {
		t.Fatalf("列宽内的帧应正常写入，得到 (%+v, %v)", result, err)
	}
}

// maxContentEditBytes 是从 DDL 抄来的常量，所以它必须和 DDL 一致。若哪天有人把
// content_edit 迁成 MEDIUMTEXT 而忘了改这里，闸会继续按 64 KiB 拒绝本来能存下的帧
// —— 一个静默收紧。反过来若列被改窄，闸会放过存不下的帧。两个方向都由这条测试挡住。
func TestMaxContentEditBytesMatchesTheMigrationChain(t *testing.T) {
	widths := map[string]int{
		"TINYTEXT": 255, "TEXT": 65535, "MEDIUMTEXT": 16777215, "LONGTEXT": 4294967295,
	}

	// 按文件名顺序扫全链，取最后一次声明为准 —— 后面的 MODIFY/CHANGE 会覆盖前面的
	// ADD COLUMN，只看第一条就会把已经迁宽过的列读成原始宽度。
	scan := func(t *testing.T, dir, column string) int {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
				names = append(names, entry.Name())
			}
		}
		// os.ReadDir 已按文件名排序，迁移文件名以日期开头，故等于时间顺序。
		pattern := regexp.MustCompile(`(?i)` + "`?" + regexp.QuoteMeta(column) + "`?" +
			`\s+(TINYTEXT|TEXT|MEDIUMTEXT|LONGTEXT)\b`)
		width := 0
		for _, name := range names {
			body, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
				w, ok := widths[strings.ToUpper(match[1])]
				if !ok {
					t.Fatalf("%s: 未知的 TEXT 变体 %q", name, match[1])
				}
				width = w
			}
		}
		if width == 0 {
			t.Fatalf("在 %s 里找不到 %s 的列声明", dir, column)
		}
		return width
	}

	for _, tc := range []struct{ name, dir, column string }{
		{"message_extra.content_edit", "../../modules/message/sql", "content_edit"},
		{"octo_message_card_revision.content", "../../modules/message/sql", "content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scan(t, tc.dir, tc.column); got != maxContentEditBytes {
				t.Fatalf("%s 的 DDL 宽度是 %d B，maxContentEditBytes 是 %d B —— "+
					"迁移改了列宽就要同步这个常量（改宽而不改常量会静默收紧，改窄而不改会放过存不下的帧）",
					tc.name, got, maxContentEditBytes)
			}
		})
	}
}
