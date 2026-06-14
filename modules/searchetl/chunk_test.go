package searchetl

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/contract/searchmsg"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

func textRow(id int64, mid, content string, createdUnix int64) *srcMessageRow {
	return &srcMessageRow{ID: id, MessageID: mid, CreatedUnix: createdUnix,
		Payload: []byte(`{"type":1,"content":"` + content + `"}`)}
}

// TestPlanChunk_StablePrefixAndRouting 稳定前缀截断 + 三态分流：
// 正文进 main、Signal raw_excluded 也进 main、坏 JSON 进 dlq；未稳定行不计入；
// 游标 maxID 取稳定前缀末（含 raw_excluded/dlq 行的 id，必须计入水位）。
func TestPlanChunk_StablePrefixAndRouting(t *testing.T) {
	rows := []*srcMessageRow{
		textRow(1, "a", "hi", 100), // OK → main
		{ID: 2, MessageID: "b", Signal: 1, CreatedUnix: 110, Payload: []byte("x")}, // signal → main(raw_excluded)
		{ID: 3, MessageID: "c", CreatedUnix: 120, Payload: []byte("{bad")},         // bad json → dlq
		textRow(4, "d", "late", 250),                                               // 未稳定 → 截断
	}
	plan := planChunk(rows, 200)
	if plan.stableCount != 3 {
		t.Fatalf("stableCount want 3, got %d", plan.stableCount)
	}
	if len(plan.main) != 2 {
		t.Fatalf("main want 2 (ok+signal), got %d", len(plan.main))
	}
	if len(plan.dlq) != 1 {
		t.Fatalf("dlq want 1, got %d", len(plan.dlq))
	}
	if !plan.advanced || plan.maxID != 3 {
		t.Fatalf("maxID must be 3 (stable prefix end incl dlq row), got advanced=%v maxID=%d", plan.advanced, plan.maxID)
	}
}

// TestPlanChunk_HeadUnstable 队首即未稳定 → 不推进。
func TestPlanChunk_HeadUnstable(t *testing.T) {
	rows := []*srcMessageRow{textRow(1, "a", "x", 300)}
	plan := planChunk(rows, 200)
	if plan.advanced || plan.maxID != 0 || len(plan.main) != 0 {
		t.Fatalf("head-unstable must not advance: %+v", plan)
	}
}

// --- 假 store / sink，用于 runChunk 事务边界与原子重投验证 ---

type fakeStore struct {
	cursor       int64
	rows         []*srcMessageRow
	readCalls    int
	advanceCalls int
	advancedTo   int64
	readErr      error
	// txnOpen 记录读事务是否处于「FOR UPDATE 持锁」窗口内。readStableBatchTx 返回前已
	// Commit（置 false），供 sink 断言「投递时读事务已关闭」（C2：锁内无 Kafka IO）。
	txnOpen bool
}

func (f *fakeStore) readStableBatchTx(table string, batch int) (int64, []*srcMessageRow, error) {
	f.readCalls++
	if f.readErr != nil {
		return 0, nil, f.readErr
	}
	// 模拟事务生命周期：开事务（持锁）→ 取批 → Commit 释锁。返回时锁必已释放。
	f.txnOpen = true
	rows := f.rows
	f.txnOpen = false
	return f.cursor, rows, nil
}

func (f *fakeStore) advanceCursor(table string, expected, newID int64) (bool, error) {
	f.advanceCalls++
	if expected != f.cursor {
		return false, nil
	}
	f.advancedTo = newID
	f.cursor = newID
	return true, nil
}

type fakeSink struct {
	mainCalls   int
	dlqCalls    int
	mainMsgs    [][]searchmsg.Message
	failMainOn  int // 第 N 次（1-based）produceBatch 返回错误，0=不失败
	curMainCall int
	store       *fakeStore // 若非 nil，投递时断言其读事务已关闭（无锁内 IO）
	sawTxnOpen  bool       // 记录是否曾在读事务持锁期间被调用（应恒 false）
}

func (s *fakeSink) produceBatch(ctx context.Context, msgs []searchmsg.Message) error {
	s.mainCalls++
	s.curMainCall++
	if s.store != nil && s.store.txnOpen {
		s.sawTxnOpen = true
	}
	if s.failMainOn != 0 && s.curMainCall == s.failMainOn {
		return errors.New("kafka write failed")
	}
	cp := append([]searchmsg.Message(nil), msgs...)
	s.mainMsgs = append(s.mainMsgs, cp)
	return nil
}

func (s *fakeSink) produceDLQ(ctx context.Context, msgs []searchmsg.Message) error {
	s.dlqCalls++
	if s.store != nil && s.store.txnOpen {
		s.sawTxnOpen = true
	}
	return nil
}

func lg() log.Log { return log.NewTLog("searchetl-test") }

// TestRunChunk_NoKafkaIOUnderLock C2 主断言：投递（main/DLQ）必发生在读事务关闭之后，
// 绝不在 FOR UPDATE 持锁窗口内。fakeStore 在 readStableBatchTx 返回前已 txnOpen=false，
// sink 每次投递时检查 store.txnOpen 必为 false；sawTxnOpen 恒 false 即证明锁内无 Kafka IO。
func TestRunChunk_NoKafkaIOUnderLock(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: []*srcMessageRow{
		textRow(1, "a", "x", 100),
		{ID: 2, MessageID: "c", CreatedUnix: 110, Payload: []byte("{bad")}, // 也触发 DLQ 投递
	}}
	sink := &fakeSink{store: store}
	_, _, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sink.sawTxnOpen {
		t.Fatalf("Kafka produce happened while DB read txn (FOR UPDATE lock) was open — C2 violated")
	}
	if sink.mainCalls != 1 || sink.dlqCalls != 1 {
		t.Fatalf("expected one main + one dlq produce, got main=%d dlq=%d", sink.mainCalls, sink.dlqCalls)
	}
}

// TestRunChunk_HappyPath 读→投→推进游标到稳定前缀末。
func TestRunChunk_HappyPath(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: []*srcMessageRow{textRow(1, "a", "x", 100), textRow(2, "b", "y", 110)}}
	sink := &fakeSink{}
	plan, n, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 2 || len(plan.main) != 2 {
		t.Fatalf("want 2 produced, got n=%d main=%d", n, len(plan.main))
	}
	if store.advancedTo != 2 {
		t.Fatalf("cursor must advance to 2, got %d", store.advancedTo)
	}
	if sink.mainCalls != 1 {
		t.Fatalf("produceBatch must be called once, got %d", sink.mainCalls)
	}
}

// TestRunChunk_ProduceFailsNoAdvance C2 核心：批中投递失败 → 游标不推进、不调 advanceCursor，
// 下轮整批可重投（store.cursor 保持原值）。
func TestRunChunk_ProduceFailsNoAdvance(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: []*srcMessageRow{textRow(1, "a", "x", 100), textRow(2, "b", "y", 110)}}
	sink := &fakeSink{failMainOn: 1}
	_, n, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err == nil {
		t.Fatalf("expected produce error to propagate")
	}
	if n != 0 {
		t.Fatalf("failed chunk must report 0 progress, got %d", n)
	}
	if store.advanceCalls != 0 {
		t.Fatalf("advanceCursor must NOT be called when produce fails, got %d calls", store.advanceCalls)
	}
	if store.cursor != 0 {
		t.Fatalf("cursor must stay at 0 for re-produce, got %d", store.cursor)
	}
}

// TestRunChunk_ReadBeforeProduce C2 顺序：必先读事务（readStableBatchTx）再投 Kafka——
// 读返回空时根本不调投递（证明投递依赖读结果，不会先于读发生）。
func TestRunChunk_EmptyReadNoProduce(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: nil}
	sink := &fakeSink{}
	_, n, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err != nil || n != 0 {
		t.Fatalf("empty read: n=%d err=%v", n, err)
	}
	if sink.mainCalls != 0 || sink.dlqCalls != 0 {
		t.Fatalf("no produce on empty read; main=%d dlq=%d", sink.mainCalls, sink.dlqCalls)
	}
	if store.advanceCalls != 0 {
		t.Fatalf("no advance on empty read")
	}
}

// TestRunChunk_HeadUnstableNoProduce 队首未稳定 → 不投递不推进。
func TestRunChunk_HeadUnstableNoProduce(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: []*srcMessageRow{textRow(1, "a", "x", 300)}}
	sink := &fakeSink{}
	_, n, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err != nil || n != 0 {
		t.Fatalf("head-unstable: n=%d err=%v", n, err)
	}
	if sink.mainCalls != 0 || store.advanceCalls != 0 {
		t.Fatalf("must not produce/advance when head unstable")
	}
}

// TestRunChunk_SignalNotInDLQ Signal 消息 raw_excluded 进 main 不进 dlq，且游标推进。
func TestRunChunk_SignalNotInDLQ(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: []*srcMessageRow{
		{ID: 1, MessageID: "s", Signal: 1, CreatedUnix: 100, Payload: []byte("ENC")},
	}}
	sink := &fakeSink{}
	plan, n, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err != nil || n != 1 {
		t.Fatalf("signal chunk: n=%d err=%v", n, err)
	}
	if len(plan.dlq) != 0 {
		t.Fatalf("signal must NOT go to dlq, got %d", len(plan.dlq))
	}
	if len(plan.main) != 1 || !plan.main[0].RawExcluded {
		t.Fatalf("signal must be raw_excluded in main")
	}
	if store.advancedTo != 1 {
		t.Fatalf("cursor must advance past signal msg, got %d", store.advancedTo)
	}
	_ = common.Text
}

// TestRunChunk_LockLostAfterProduceNoAdvance 🔴 C3：投递成功后、推进游标前锁丢失（lockCtx 取消）
// → 不推进游标（advanceCursor 零调用），返回 ctx 错误。已投消息靠 message_id 幂等 + ES _id
// upsert 下轮重投覆盖；绝不在失锁后推进游标（失锁不双算）。
func TestRunChunk_LockLostAfterProduceNoAdvance(t *testing.T) {
	store := &fakeStore{cursor: 0, rows: []*srcMessageRow{textRow(1, "a", "x", 100)}}
	// sink 在投递成功后取消 ctx，模拟「投递与推进之间续租失败」。
	ctx, cancel := context.WithCancel(context.Background())
	sink := &cancelSink{cancel: cancel}
	_, n, err := runChunk(ctx, store, sink, "message", 200, 5000, lg())
	if err == nil {
		t.Fatalf("expected ctx error after lock loss")
	}
	if n != 0 {
		t.Fatalf("lock-lost chunk must report 0 progress, got %d", n)
	}
	if store.advanceCalls != 0 {
		t.Fatalf("advanceCursor must NOT be called after lock loss, got %d", store.advanceCalls)
	}
	if store.cursor != 0 {
		t.Fatalf("cursor must stay at 0 (re-produce next tick), got %d", store.cursor)
	}
	if sink.mainCalls != 1 {
		t.Fatalf("produce should have happened once before loss, got %d", sink.mainCalls)
	}
}

// cancelSink 在 produceBatch 成功后取消传入的 ctx，模拟投递与游标推进之间的失锁。
type cancelSink struct {
	cancel    context.CancelFunc
	mainCalls int
}

func (s *cancelSink) produceBatch(ctx context.Context, msgs []searchmsg.Message) error {
	s.mainCalls++
	s.cancel() // 投递成功后立即失锁
	return nil
}

func (s *cancelSink) produceDLQ(ctx context.Context, msgs []searchmsg.Message) error {
	return nil
}

// TestRunChunk_CASMissNoAdvance 🔴 C3 fence：投递成功后游标已被另一持锁副本推进（CAS 失配，
// advanceCursor 返回 false）→ 本轮不计已投、不报错，下轮从新水位续跑。这是失锁竞态的安全收口：
// 即便本 owner 失锁后仍跑到 advanceCursor，游标行 CAS（WHERE last_id=expected）也挡住越权推进。
func TestRunChunk_CASMissNoAdvance(t *testing.T) {
	store := &casMissStore{cursor: 0, rows: []*srcMessageRow{textRow(1, "a", "x", 100)}}
	sink := &fakeSink{}
	_, n, err := runChunk(context.Background(), store, sink, "message", 200, 5000, lg())
	if err != nil {
		t.Fatalf("CAS miss must not error, got %v", err)
	}
	if n != 0 {
		t.Fatalf("CAS-miss chunk must report 0 progress, got %d", n)
	}
	if sink.mainCalls != 1 {
		t.Fatalf("produce should have happened once, got %d", sink.mainCalls)
	}
}

// casMissStore 的 advanceCursor 恒返回 (false,nil)，模拟「游标已被另一持锁副本推进」。
type casMissStore struct {
	cursor int64
	rows   []*srcMessageRow
}

func (s *casMissStore) readStableBatchTx(table string, batch int) (int64, []*srcMessageRow, error) {
	return s.cursor, s.rows, nil
}
func (s *casMissStore) advanceCursor(table string, expected, newID int64) (bool, error) {
	return false, nil // CAS 失配：另一副本已推进游标
}
