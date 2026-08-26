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

// MaxPersistedFrameBytes 是发送侧（modules/bot_api 模板分支）做同口径预检用的导出
// 别名。它必须等于闸自己用的那个数 —— 两边一旦分叉，发送预检就会放过编辑一定会拒的帧，
// 「发得出去、第一次编辑就被拒」这个失败模式原样回来。review 指出这个常量此前零测试
// 覆盖（P2-2），而它所在的文件正是本轮 blocker 出问题的地方。
func TestMaxPersistedFrameBytesTracksTheGate(t *testing.T) {
	if MaxPersistedFrameBytes != maxContentEditBytes {
		t.Fatalf("MaxPersistedFrameBytes = %d, 闸用的 maxContentEditBytes = %d —— "+
			"发送侧预检与写入侧闸口径分叉了", MaxPersistedFrameBytes, maxContentEditBytes)
	}
	// 自证这个别名真的是闸的判据：正好等于它的帧放行，多一个字节被拒。
	backend := &fakeMutationBackend{}
	if _, err := newCardMutator(backend).WriteCAS(CardMutationCASRequest{
		MessageID: "1001", MessageSeq: 7, ChannelID: "user-b", ChannelType: 1,
		ContentEdit: strings.Repeat("x", MaxPersistedFrameBytes), ContentHash: "h", EditedAt: 1, CardSeq: 2,
	}); err != nil {
		t.Fatalf("MaxPersistedFrameBytes 个字节应放行，得到 %v", err)
	}
	if _, err := newCardMutator(&fakeMutationBackend{}).WriteCAS(CardMutationCASRequest{
		MessageID: "1001", MessageSeq: 7, ChannelID: "user-b", ChannelType: 1,
		ContentEdit: strings.Repeat("x", MaxPersistedFrameBytes+1), ContentHash: "h", EditedAt: 1, CardSeq: 2,
	}); !errors.Is(err, ErrCardMutationTooLarge) {
		t.Fatalf("MaxPersistedFrameBytes+1 个字节应被拒，得到 %v", err)
	}
}

// NormalizeFrameForPersistence 的文档注释逐条列出了它的全部使用点（五条写路径 + 一处发送侧
// 预检）。这类枚举会漂移，
// 而一段断言了不成立的不变量的注释正是 #712 里两轮 blocker 的成因（注释说「所有回写路径
// 都走这个判据」，而 raw 编辑分支和 WriteCAS 都不走）。所以这里用源码守卫钉住计数：调用点
// 增减时测试失败，逼着改注释，而不是等下一个人相信一段过时的断言。
//
// **这条守卫证明的是什么、不是什么**（review of #716 要求写清）：它只保证那段注释里的计数与
// 源码里判据的使用点数量一致 —— 也就是「注释别撒谎」。它**无法**发现一条绕过判据直接写库的
// 路径，那种缺陷按构造就在「数调用点」这个手段的射程之外，无论扫多少目录。#712 的 P0 正是那
// 一类，只能靠人枚举写入口（见 NormalizeFrameForPersistence 注释里的「刻意不在覆盖面内」段）。
//
// 扫描范围是整个 module（递归），所以在第四个包或子包里新增调用也会被看见 —— 早先的版本只扫
// 三个硬编码目录且非递归，那时新包是隐形的。
func TestPersistenceJudgeCallSitesMatchItsDocumentedCount(t *testing.T) {
	// 注释里声明的条数。改这个数就必须同步改那段枚举 —— 反过来也一样：这条守卫在本次
	// follow-up 里真的响过一次（新增模板发送预检后实测 6 条 vs 声明 5 条），逼着把第 6 条
	// 补进注释并说明它是「不落库、只借判据」的那一类。守卫有效性由此自证。
	const documented = 7

	// 递归扫整个 module，而不是三个硬编码目录 —— 第四个包/子包里的调用不能是隐形的。
	const repoRoot = "../.."
	callRE := regexp.MustCompile(`NormalizeFrameForPersistence\(`)
	// WriteCAS 自己查宽度、不经过那个函数，所以按注释里的「独立设闸」单独计。两个常量名
	// 都接受：写死一个会让改用导出别名时报出「你增删了写路径」这种误导性失败。
	standaloneRE := regexp.MustCompile(`>\s*(maxContentEditBytes|MaxPersistedFrameBytes)\b`)

	sites := 0
	standalone := 0
	if err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "assets", "docs", ".octospec":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// 判据**自己**函数体内的那次宽度检查不算「另一处设闸」—— 它就是判据本身。用
		// 「见到声明起、见到列首 } 止」跳过它；靠变量名区分（len(normalized) vs
		// len(request.ContentEdit)）会让重命名接收者变量就误报「你增删了写路径」。
		inJudgeBody := false
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "func NormalizeFrameForPersistence") {
				inJudgeBody = true
				continue
			}
			if inJudgeBody {
				if line == "}" {
					inJudgeBody = false
				}
				continue
			}
			// 行注释不算使用点。块注释 / 字符串字面量里的同名文本仍会被算进来 —— 这是已知
			// 的粗糙处，彻底解决要用 go/ast；当前树里没有这种写法，且方向是 fail-closed
			// （多算只会让计数不符而报错，不会漏算）。
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if callRE.MatchString(line) {
				sites++
			}
			if standaloneRE.MatchString(line) {
				standalone++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}

	if got := sites + standalone; got != documented {
		t.Fatalf("持久化判据的使用点实测 %d 处（%d 处调用 + %d 处独立设闸），注释声明 %d 处。\n"+
			"增删使用点就要同步 NormalizeFrameForPersistence 的注释枚举，并标明新增那处是\n"+
			"「写路径」还是「不落库的预检」。注意本守卫的射程：它只保证注释里的计数不撒谎，\n"+
			"发现不了绕过判据直接写库的路径 —— 那种缺陷（#712 的 P0）只能靠人枚举写入口。",
			got, sites, standalone, documented)
	}
}
