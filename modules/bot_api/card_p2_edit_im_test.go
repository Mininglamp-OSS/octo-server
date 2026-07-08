package bot_api

// card-message-interaction P2 D6/D9 集成测试（需 WuKongIM :5001）。
// spec: .octospec/tasks/card-message-interaction/brief.md；执行 brief:
// .octospec/tasks/card-message-p2-action-loop/brief.md。
//
// 覆盖：bot 卡片编辑解锁（D6 整卡替换 + 权威 plain 重算）→ 跨类型变异拒绝
// （D6 不变量 a）→ card_seq CAS 乱序帧拒绝（D9）→ 脏帧白名单拒绝。
// send 响应只带 message_id（官方 WuKongIM v2 语义），编辑前经 IMSearchMessages
// 轮询等 message_seq 就绪。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

const (
	imCardBotID    = "bot_card_im"
	imCardBotToken = "bf_card_im_token"
)

// imCardEnvelope 构造 octo/v2 卡片信封；cardSeq<0 表示不带 card_seq。
func imCardEnvelope(actionID string, cardSeq int64) map[string]interface{} {
	env := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"plain":        "forged-by-client",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "审批单 #7 状态卡"},
			},
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.Submit", "id": actionID, "title": actionID},
			},
		},
	}
	if cardSeq >= 0 {
		env["card_seq"] = cardSeq
	}
	return env
}

func TestBotCardEditCASIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", imCardBotID, imCardBotToken).Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{imCardBotID, testutil.UID}, {testutil.UID, imCardBotID}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}

	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer "+imCardBotToken)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// ① bot 发卡（真实 IM 派发）
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      imCardEnvelope("approve_btn", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	assert.NotZero(t, sendResp.MessageID)
	msgID := fmt.Sprintf("%d", sendResp.MessageID)

	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  []int64{sendResp.MessageID},
			LoginUID:    imCardBotID,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq, "消息未完成 IM 异步持久化")

	editBody := func(env map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"message_id":   msgID,
			"message_seq":  msgSeq,
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": util.ToJson(env),
		}
	}

	// ② D6 happy：整卡替换为新帧（card_seq=2），校验 + plain 权威重算 + 落库
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("done_btn", 2)))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var stored string
	err = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&stored)
	assert.NoError(t, err)
	assert.Contains(t, stored, "done_btn", "message_extra 应存最新帧")
	assert.NotContains(t, stored, "forged-by-client", "plain 必须被服务端重算覆盖")
	// P1-2：捕获 advancing 写后的 version,用于验证下方冲突帧不改动它。
	var vAfterDone int64
	err = ctx.DB().Select("version").From("message_extra").Where("message_id=?", msgID).LoadOne(&vAfterDone)
	assert.NoError(t, err)

	// ③ D9 CAS：乱序帧（card_seq=1 ≤ 已存 2）→ 冲突（D14 线上 400 + 文案），不覆盖
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("stale_btn", 1)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card update rejected: stale card_seq.")
	var after string
	_ = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&after)
	assert.Contains(t, after, "done_btn", "乱序帧不得覆盖已存帧")
	// P1-2：冲突帧什么都不写 —— version 必须原样不动(GenSeq 在 CAS 冲突判定之后,
	// 乱序帧不消费/推进 version,避免平白推进 delta-sync 游标)。
	var vAfterStale int64
	err = ctx.DB().Select("version").From("message_extra").Where("message_id=?", msgID).LoadOne(&vAfterStale)
	assert.NoError(t, err)
	assert.Equal(t, vAfterDone, vAfterStale, "冲突帧不得改动 version")

	// ④ D6 跨类型变异：卡片消息被"编辑"为纯文本体 → 拒绝
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id":   msgID,
		"message_seq":  msgSeq,
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": "plain text takeover",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid request.")

	// ⑤ 脏卡片帧（javascript: URL）→ 白名单拒绝，不落库
	dirty := imCardEnvelope("x_btn", 3)
	dirty["card"].(map[string]interface{})["body"] = []interface{}{
		map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
	}
	w = do("/v1/bot/message/edit", editBody(dirty))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card payload.")
	var count int
	_ = ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit like ?", msgID, "%javascript%").LoadOne(&count)
	assert.Zero(t, count, "脏帧不得落库")

	// ⑥ 同 seq 不同内容（seq=2，已存 done_btn/seq=2）→ 409（PR#548 review 非阻塞项：
	//    证明 content_edit_hash 去重不掩盖 D9 stale 冲突 —— 内容不同则 hash 不同、不
	//    命中去重、走 CAS，2 ≤ 2 得冲突）。
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("other_btn", 2)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card update rejected: stale card_seq.")

	// ⑦ 完全相同帧重发（done_btn/seq=2）→ OK 幂等（hash 去重命中，正是应有行为，
	//    不是被掩盖的 stale 冲突）。
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("done_btn", 2)))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestBotCardEditConcurrentCASIM 验证 D9 CAS 在并发下无 lost-update：并发发若干
// 递增 card_seq 的编辑帧,不论到达顺序,最终 stored 必为最大 seq 那一帧 —— 一旦
// 最大 seq 帧落库,任何更小 seq 都被拒；而最大 seq 帧到达时 stored 必 < 它,故必被
// 应用。SELECT ... FOR UPDATE 的 next-key 锁把并发首帧也串行化。
func TestBotCardEditConcurrentCASIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const casBot = "bot_card_cas"
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", casBot, "bf_card_cas_token").Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{casBot, testutil.UID}, {testutil.UID, casBot}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}
	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer bf_card_cas_token")
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"payload": imCardEnvelope("f0", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	msgID := fmt.Sprintf("%d", sendResp.MessageID)
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID: testutil.UID, ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds: []int64{sendResp.MessageID}, LoginUID: casBot,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq)

	const n = 8
	// P1-2（PR#548 review）：并发下的 version 单调性。delta-sync 按 version 升序取
	// 增量帧,若某帧以更小 version 覆盖行,已同步到更高 version 的客户端将永远收不到
	// 该帧。best-effort poller 采样行 version,记录是否回退与采样到的最大值;修复前
	// (version 锁外前置分配)可观测到回退或最终 version < 采样峰值。
	var (
		pollMu       sync.Mutex
		maxSeen      int64
		lastSeen     int64
		wentBackward bool
	)
	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			var v int64
			if e := ctx.DB().Select("version").From("message_extra").Where("message_id=?", msgID).LoadOne(&v); e == nil {
				pollMu.Lock()
				if v < lastSeen {
					wentBackward = true
				}
				lastSeen = v
				if v > maxSeen {
					maxSeen = v
				}
				pollMu.Unlock()
			}
			time.Sleep(200 * time.Microsecond) // 稀释采样:避免与并发写者抢连接池/空转
		}
	}()

	var wg sync.WaitGroup
	for seq := 1; seq <= n; seq++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			do("/v1/bot/message/edit", map[string]interface{}{
				"message_id": msgID, "message_seq": msgSeq,
				"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
				"content_edit": util.ToJson(imCardEnvelope(fmt.Sprintf("f%d", seq), int64(seq))),
			})
		}(seq)
	}
	wg.Wait()
	close(stopPoll)
	pollWG.Wait()

	// 不变量:最终 stored 必为最大 seq(n)的帧,不论并发到达顺序。
	var storedSeq int64
	err = ctx.DB().Select("card_seq").From("message_extra").Where("message_id=?", msgID).LoadOne(&storedSeq)
	assert.NoError(t, err)
	assert.Equal(t, int64(n), storedSeq, "并发 CAS 后最终 card_seq 必为最大值(无 lost-update/stale-overwrite)")
	var stored string
	_ = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&stored)
	assert.Contains(t, stored, fmt.Sprintf("f%d", n), "最终帧必为最大 seq 那一帧")

	// P1-2 version 单调性:行 version 不得回退,且最终 version 必 ≥ 采样峰值(赢家帧 =
	// 最后一次 advancing 写 = 最大 version;否则终帧对 delta-sync 不可见)。
	var finalVersion int64
	err = ctx.DB().Select("version").From("message_extra").Where("message_id=?", msgID).LoadOne(&finalVersion)
	assert.NoError(t, err)
	assert.False(t, wentBackward, "message_extra.version 不得回退(P1-2:CAS 必须锁内分配单调 version)")
	assert.GreaterOrEqual(t, finalVersion, maxSeen, "最终 version 必 ≥ 采样峰值(赢家帧 version 最大)")
}

// TestBotCardEditMixedFrameVersionMonotonicIM 验证 P1（PR#548 review round-3）：无
// card_seq 的编辑帧(LWW,非 CAS 分支)也必须在行锁内分配 version。并发混发 card_seq
// 帧与无 card_seq 帧时,两分支取同一把 message_id 行锁,version 分配序 == 提交序,行
// version 绝不回退 —— delta-sync(version>? 游标)不丢终帧。修复前非 CAS 分支锁外前置
// 分配 version,低 version 的无 card_seq 帧后提交会覆盖并发 CAS 帧已提交的高 version。
// TestBotCardEditMixedFrameVersionMonotonicIM 验证混发 CAS/非 CAS 帧时 version 不回退
// (finalVersion ≥ 采样峰值) —— 但**仅覆盖单进程内**保证(PR#548 review H2/P1-b)：本测试跑
// 在单个进程里、共享同一个 GenSeq HiLo 分配器,验证的是「锁内分配使分配序==提交序」关掉的
// **进程内**竞态。跨副本单调性本测试观测不到(GenSeq 进程级号段,多实例可回退),那是既有性质、
// 需频道级全序源才能根治,超出 #548 —— 详见 send.go cardVersionInLockWrite 注释 H2/P1-b。
func TestBotCardEditMixedFrameVersionMonotonicIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const mixBot = "bot_card_mix"
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", mixBot, "bf_card_mix_token").Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{mixBot, testutil.UID}, {testutil.UID, mixBot}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}
	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer bf_card_mix_token")
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"payload": imCardEnvelope("m0", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	msgID := fmt.Sprintf("%d", sendResp.MessageID)
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID: testutil.UID, ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds: []int64{sendResp.MessageID}, LoginUID: mixBot,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq)

	var (
		pollMu       sync.Mutex
		maxSeen      int64
		lastSeen     int64
		wentBackward bool
	)
	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			var v int64
			if e := ctx.DB().Select("version").From("message_extra").Where("message_id=?", msgID).LoadOne(&v); e == nil {
				pollMu.Lock()
				if v < lastSeen {
					wentBackward = true
				}
				lastSeen = v
				if v > maxSeen {
					maxSeen = v
				}
				pollMu.Unlock()
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	const n = 8
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 奇数:带递增 card_seq(CAS 分支);偶数:无 card_seq(LWW 非 CAS 分支)。
			seq := int64(-1)
			if i%2 == 1 {
				seq = int64(i)
			}
			do("/v1/bot/message/edit", map[string]interface{}{
				"message_id": msgID, "message_seq": msgSeq,
				"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
				"content_edit": util.ToJson(imCardEnvelope(fmt.Sprintf("m%d", i), seq)),
			})
		}(i)
	}
	wg.Wait()
	close(stopPoll)
	pollWG.Wait()

	var finalVersion int64
	err = ctx.DB().Select("version").From("message_extra").Where("message_id=?", msgID).LoadOne(&finalVersion)
	assert.NoError(t, err)
	pollMu.Lock()
	defer pollMu.Unlock()
	assert.False(t, wentBackward, "单进程内混发 CAS/非 CAS 帧下 version 观测到回退(非 CAS 分支锁外分配的 lost-update)")
	assert.GreaterOrEqual(t, finalVersion, maxSeen, "单进程内最终 version 必 ≥ 采样峰值(无低 version 覆盖高 version;跨副本不保证,见函数注释)")
}

// TestBotCardEditOwnershipAndLifecycleIM 验收 PR#548 review 两项补强:
// ① P0 —— message_id/message_seq 不匹配硬拒绝。所有权只在 (channel, seq) 上验证,
//
//	但写入按调用方另给的 message_id 落 UNIQUE(message_id) 单表 → warn-only 会形成
//	confused-deputy(攻击 bot 用自己拥有的 seq + 他人 message_id 覆盖他人卡片的
//	content_edit)。断言:不匹配被拒,且 foreign 行不被写入。
//
// ② P2 撤回门禁 —— 已撤回/删除的卡片不可再编辑(与动作端点撤回门禁对称)。
func TestBotCardEditOwnershipAndLifecycleIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const bot = "bot_card_own"
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", bot, "bf_card_own_token").Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{bot, testutil.UID}, {testutil.UID, bot}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}
	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer bf_card_own_token")
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// bot 发自己的卡,拿到真实 message_id + seq
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"payload": imCardEnvelope("approve_btn", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	ownID := fmt.Sprintf("%d", sendResp.MessageID)
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID: testutil.UID, ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds: []int64{sendResp.MessageID}, LoginUID: bot,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq)

	// ① P0:用拥有的 seq + 一个不属于自己的 message_id 编辑 → 硬拒绝,foreign 行不被写。
	const foreignID = "9223372036854775000"
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id": foreignID, "message_seq": msgSeq,
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": util.ToJson(imCardEnvelope("attacker_btn", 5)),
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "message_id/message_seq 不匹配必须拒绝")
	var foreignCnt int
	_ = ctx.DB().Select("count(*)").From("message_extra").Where("message_id=?", foreignID).LoadOne(&foreignCnt)
	assert.Zero(t, foreignCnt, "confused-deputy:foreign message_id 行不得被写入")

	// ② P2 撤回门禁:把自己的卡标记撤回,再用正确 id+seq 编辑 → 拒绝,content_edit 不落库。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,`revoke`,version) VALUES (?,?,?,?,?,?)",
		ownID, msgSeq, common.GetFakeChannelIDWith(bot, testutil.UID), common.ChannelTypePerson.Uint8(), 1, 1).Exec()
	assert.NoError(t, err)
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id": ownID, "message_seq": msgSeq,
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": util.ToJson(imCardEnvelope("done_btn", 2)),
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "已撤回卡片不可编辑")
	var edited string
	_ = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", ownID).LoadOne(&edited)
	assert.NotContains(t, edited, "done_btn", "撤回卡片编辑不得写入 content_edit")
}

// TestBotCardRevisionsIM 验证 D10：非 transient 卡片编辑追加修订帧、transient 帧不
// 入历史、清除端点删帧 + 写墓碑（需 WuKongIM）。
func TestBotCardRevisionsIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const revBot, revTok = "bot_card_rev", "bf_card_rev_token"
	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,bot_token,status) values(?,?,1)", revBot, revTok).Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{revBot, testutil.UID}, {testutil.UID, revBot}} {
		_, ferr := ctx.DB().InsertBySql("insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}
	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer "+revTok)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	frameCount := func(msgID string) int {
		var n int
		_ = ctx.DB().Select("count(*)").From("octo_message_card_revision").Where("message_id=? and is_tombstone=0", msgID).LoadOne(&n)
		return n
	}
	tombstoneCount := func(msgID string) int {
		var n int
		_ = ctx.DB().Select("count(*)").From("octo_message_card_revision").Where("message_id=? and is_tombstone=1", msgID).LoadOne(&n)
		return n
	}

	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"payload": imCardEnvelope("s0", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	msgID := fmt.Sprintf("%d", sendResp.MessageID)
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID: testutil.UID, ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds: []int64{sendResp.MessageID}, LoginUID: revBot,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq)
	edit := func(env map[string]interface{}) *httptest.ResponseRecorder {
		return do("/v1/bot/message/edit", map[string]interface{}{
			"message_id": msgID, "message_seq": msgSeq,
			"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": util.ToJson(env),
		})
	}

	// ① 非 transient 编辑 → 追加 1 帧。
	assert.Equal(t, http.StatusOK, edit(imCardEnvelope("s1", 2)).Code)
	assert.Equal(t, 1, frameCount(msgID))

	// ② transient 编辑 → content_edit 更新但不入历史（仍 1 帧）。
	transient := imCardEnvelope("s2", 3)
	transient["transient"] = true
	assert.Equal(t, http.StatusOK, edit(transient).Code)
	assert.Equal(t, 1, frameCount(msgID), "transient 帧不入修订历史")

	// ③ 再一次非 transient 编辑 → 2 帧。
	assert.Equal(t, http.StatusOK, edit(imCardEnvelope("s3", 4)).Code)
	assert.Equal(t, 2, frameCount(msgID))

	// ④ 清除端点：删帧 + 写墓碑。
	w = do("/v1/bot/message/card/revisions/clear", map[string]interface{}{
		"message_id": msgID, "channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"cleared":2`)
	assert.Equal(t, 0, frameCount(msgID), "清除后无帧")
	assert.Equal(t, 1, tombstoneCount(msgID), "清除写一条墓碑")
}
