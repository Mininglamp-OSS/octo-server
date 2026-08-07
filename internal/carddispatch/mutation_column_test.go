package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
//
// 扫描必须**限定到具名表**。modules/message/sql 下有三张不同的表各自声明了一个裸
// `content TEXT`（20210305000001 / 20220810000001 / 20250708000002），不限定表就是按
// 文件名顺序取最后一条 —— 今天恰好命中对的那张纯属排序运气。一旦某次迁宽落在**别的**
// 表上，扫描会读出 16 MB，这条测试失败，而它的失败消息正是「去同步这个常量」，照做就
// 会把闸放宽到 16 MB 对着一个 64 KiB 的列，`Data too long` 原样回来。反向陷阱比不检查
// 更糟（PR#712 review P2-6）。
func TestMaxContentEditBytesMatchesTheMigrationChain(t *testing.T) {
	widths := map[string]int{
		"TINYTEXT": 255, "TEXT": 65535, "MEDIUMTEXT": 16777215, "LONGTEXT": 4294967295,
	}

	// 按文件名顺序扫全链，取最后一次声明为准 —— 后面的 MODIFY/CHANGE 会覆盖前面的
	// ADD COLUMN，只看第一条就会把已经迁宽过的列读成原始宽度。
	scan := func(dir, table, column string) (int, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
				names = append(names, entry.Name())
			}
		}
		// os.ReadDir 已按文件名排序，迁移文件名以日期开头，故等于时间顺序。
		quoted := "`?" + regexp.QuoteMeta(table) + "`?"
		// 每条 CREATE/ALTER 语句切成一段，只在**目标表**那些段里找列声明。
		stmtRE := regexp.MustCompile(`(?is)(CREATE\s+TABLE|ALTER\s+TABLE)\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
			"`?[A-Za-z0-9_]+`?")
		colRE := regexp.MustCompile(`(?i)` + "`?" + regexp.QuoteMeta(column) + "`?" +
			`\s+(TINYTEXT|TEXT|MEDIUMTEXT|LONGTEXT)\b`)
		targetRE := regexp.MustCompile(`(?is)(?:CREATE|ALTER)\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + quoted + `\b`)
		width := 0
		matchedTable := false
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return 0, fmt.Errorf("read %s: %w", name, err)
			}
			body := string(raw)
			starts := stmtRE.FindAllStringIndex(body, -1)
			for i, span := range starts {
				end := len(body)
				if i+1 < len(starts) {
					end = starts[i+1][0]
				}
				segment := body[span[0]:end]
				if !targetRE.MatchString(segment) {
					continue
				}
				matchedTable = true
				for _, match := range colRE.FindAllStringSubmatch(segment, -1) {
					w, ok := widths[strings.ToUpper(match[1])]
					if !ok {
						return 0, fmt.Errorf("%s: 未知的 TEXT 变体 %q", name, match[1])
					}
					width = w
				}
			}
		}
		if !matchedTable {
			return 0, fmt.Errorf("在 %s 里找不到表 %s 的 CREATE/ALTER 语句 —— 表名改了就要同步这里", dir, table)
		}
		if width == 0 {
			return 0, fmt.Errorf("在 %s 的表 %s 里找不到 %s 的 TEXT 系列列声明", dir, table, column)
		}
		return width, nil
	}

	// 自证限定生效：一个不存在的表必须让扫描失败，而不是退回去匹配别人的
	// `content TEXT`。没有这条，上面两个用例在正则退化成表盲时依然会通过。
	t.Run("scoping is active", func(t *testing.T) {
		if width, err := scan("../../modules/message/sql", "no_such_table", "content"); err == nil {
			t.Fatalf("扫描一个不存在的表返回了 %d B —— 说明表限定没生效，它匹配到了别的表的 "+
				"content 列；三张表都声明了裸 content TEXT，不限定表这条测试就只是在赌文件名顺序", width)
		}
	})

	for _, tc := range []struct{ name, dir, table, column string }{
		{"message_extra.content_edit", "../../modules/message/sql", "message_extra", "content_edit"},
		{"octo_message_card_revision.content", "../../modules/message/sql", "octo_message_card_revision", "content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scan(tc.dir, tc.table, tc.column)
			if err != nil {
				t.Fatal(err)
			}
			if got != maxContentEditBytes {
				t.Fatalf("%s 的 DDL 宽度是 %d B，maxContentEditBytes 是 %d B —— "+
					"迁移改了列宽就要同步这个常量（改宽而不改常量会静默收紧，改窄而不改会放过存不下的帧）",
					tc.name, got, maxContentEditBytes)
			}
		})
	}
}

// WriteCAS 是 raw 卡片编辑（modules/bot_api）走的写入口：调用方自己跑 cardmsg 收敛
// 和 render_profile 改写，然后把结果直接递进来。它原本没有列宽检查，于是成了绕过
// NormalizeFrameForPersistence 的通道 —— 一个 cardmsg 合法（512 KiB 上限内）但比 TEXT
// 列宽的帧直达 INSERT（PR#712 review）。闸放在这里而不是只放调用点，让未来的调用方
// 也拿不到旁路。
func TestWriteCASRefusesFramesWiderThanTheColumn(t *testing.T) {
	backend := &fakeMutationBackend{}
	mutator := newCardMutator(backend)

	oversized := strings.Repeat("x", maxContentEditBytes+1)
	_, err := mutator.WriteCAS(CardMutationCASRequest{
		MessageID: "1001", MessageSeq: 7, ChannelID: "user-b", ChannelType: 1,
		ContentEdit: oversized, ContentHash: "h", EditedAt: 1, CardSeq: 2,
	})
	if !errors.Is(err, ErrCardMutationTooLarge) {
		t.Fatalf("WriteCAS(oversized) error = %v, want ErrCardMutationTooLarge", err)
	}
	if !errors.Is(err, ErrCardMutationInvalid) {
		t.Fatalf("ErrCardMutationTooLarge must wrap ErrCardMutationInvalid; err = %v", err)
	}
	if len(backend.writes) != 0 {
		t.Fatalf("oversized frame reached CASWrite: %d writes", len(backend.writes))
	}

	// 边界另一侧：正好等于列宽的帧照常写入，闸不越界收紧。
	backend = &fakeMutationBackend{}
	if _, err := newCardMutator(backend).WriteCAS(CardMutationCASRequest{
		MessageID: "1001", MessageSeq: 7, ChannelID: "user-b", ChannelType: 1,
		ContentEdit: strings.Repeat("x", maxContentEditBytes), ContentHash: "h", EditedAt: 1, CardSeq: 2,
	}); err != nil {
		t.Fatalf("WriteCAS at exactly the column width should succeed, got %v", err)
	}
	if len(backend.writes) != 1 {
		t.Fatalf("at-limit frame should be written once, got %d", len(backend.writes))
	}
}
