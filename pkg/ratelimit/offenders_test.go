package ratelimit

import (
	"fmt"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

// TestOffenderRecorderIsStructurallyBounded 是 brief Acceptance 里
// 「offender ZSet 结构性有界」那一条的钉子。
//
// 为什么必须**结构性**有界(而不是"调用方记得清理"):这是整套观测里**唯一**持有
// 业务身份(robotID)的结构。我们之所以拒绝把 robot_id 放进 Prometheus label,
// 就是因为身份维度无上界;若在 Redis 里再犯一次,只是把爆炸点从抓取端搬到内存。
//
// 而且生产 Redis **未设 maxmemory**(无 LRU 淘汰、OOM 直接被 OS kill),
// 所以"无界集合"在这套部署里不是性能问题,是可用性问题。
func TestOffenderRecorderIsStructurallyBounded(t *testing.T) {
	c := rd.NewClient(&rd.Options{Addr: testRedisAddr(), DialTimeout: 300 * time.Millisecond})
	defer c.Close()
	if err := c.Ping().Err(); err != nil {
		t.Skipf("需要本地 Redis:%v", err)
	}

	const prefix = "test:offenders:"
	const class = "business"
	key := prefix + class
	require.NoError(t, c.Del(key).Err())
	t.Cleanup(func() { _ = c.Del(key).Err() })

	r := NewOffenderRecorder(c, prefix)

	// 灌入远超上限的成员数。每个 member 都不同,模拟"很多不同的 bot 都超限"。
	const inserted = defaultOffendersTopN * 3
	for i := 0; i < inserted; i++ {
		r.Record(class, "bot_"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}

	// 上界是**高水位**而非 topN:裁剪只在超过 highWater 时触发,这是为了不让新
	// offender 一进来就被删掉(见 TestOffenderRecorderDoesNotStarveNewOffenders)。
	// 关键性质没变——集合规模是 topN 的常数倍,仍然结构性有界。
	upperBound := defaultOffendersTopN * defaultOffendersHighWaterFactor
	card, err := c.ZCard(key).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, int(card), upperBound,
		"灌入 %d 个成员后集合应被裁剪到 ≤%d,实际 %d —— 裁剪未生效,集合无界",
		inserted, upperBound, card)

	// TTL 必须存在:不再产生拒绝的 class,其名单应自然过期消失,
	// 否则运维会看到一个早已恢复的 bot 仍挂在榜首。
	ttl, err := c.TTL(key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0), "offenders key 必须带 TTL,否则名单永不过期")
	require.LessOrEqual(t, ttl, defaultOffendersTTL)
}

// TestOffenderRecorderKeepsHighestScores 确认裁剪保留的是**最严重**的 offender。
//
// 若 ZREMRANGEBYRANK 的区间写反(删高分而非低分),集合大小同样有界、
// 上面那条断言照样通过,但名单会只剩噪声——事故时看到的是"超限最少的 50 个 bot"。
// 这种错误不会有任何症状,直到有人真的去看那份名单。
func TestOffenderRecorderKeepsHighestScores(t *testing.T) {
	c := rd.NewClient(&rd.Options{Addr: testRedisAddr(), DialTimeout: 300 * time.Millisecond})
	defer c.Close()
	if err := c.Ping().Err(); err != nil {
		t.Skipf("需要本地 Redis:%v", err)
	}

	const prefix = "test:offenders-rank:"
	const class = "heartbeat"
	key := prefix + class
	require.NoError(t, c.Del(key).Err())
	t.Cleanup(func() { _ = c.Del(key).Err() })

	r := NewOffenderRecorder(c, prefix)

	// heavy 被记很多次(高 score),然后灌入远超上限的一次性成员把它挤压。
	for i := 0; i < 20; i++ {
		r.Record(class, "heavy_bot")
	}
	for i := 0; i < defaultOffendersTopN*2; i++ {
		r.Record(class, "noise_"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}

	top, err := r.Top(class, 5)
	require.NoError(t, err)
	require.NotEmpty(t, top)
	require.Equal(t, "heavy_bot", top[0].Member,
		"裁剪应保留最高分成员;榜首不是 heavy_bot 说明 ZREMRANGEBYRANK 区间写反了")
	require.Equal(t, float64(20), top[0].Score)
}

// TestOffenderRecorderDoesNotStarveNewOffenders —— **满集合不得饿死新来的**。
//
// 这条对应一个真实 bug（三位 reviewer 里两位独立指出，并用 redis-cli 复现）:
// 初版每次写入都 `ZREMRANGEBYRANK key 0 -topN-1`。当集合已有 topN 个成员、分数都 >= 2 时,
// 一个新 offender 以 1 分进来 ⇒ 成为唯一最低分 ⇒ **同一趟 Lua 里立刻被删掉**;
// 下次再犯还是从 1 分开始、再被删掉。于是一个在 50 个之后才开始作妖的 bot
// **永远不会出现在名单里**。
//
// 为什么这不是普通观测瑕疵:整个灰度计划都依赖影子模式回答"这个配额会拒谁",
// 而"在既有 offender 之后才开始超限的 bot"恰恰是最需要看见的那一类。
//
// 上面那条 KeepsHighestScores 抓不到它——它只往集合里灌噪声,从不在**已饱和**的
// 集合里加新成员。两条测试的差别只有"集合是否已满",而 bug 只在满的时候出现。
func TestOffenderRecorderDoesNotStarveNewOffenders(t *testing.T) {
	c := rd.NewClient(&rd.Options{Addr: testRedisAddr(), DialTimeout: 300 * time.Millisecond})
	defer c.Close()
	if err := c.Ping().Err(); err != nil {
		t.Skipf("需要本地 Redis:%v", err)
	}

	const prefix = "test:offenders-starve:"
	const class = "register"
	key := prefix + class
	require.NoError(t, c.Del(key).Err())
	t.Cleanup(func() { _ = c.Del(key).Err() })

	r := NewOffenderRecorder(c, prefix)

	// 先把集合填到 topN 个成员,每个都拿到明显高于新人的分数(5 分)。
	// 这是初版会出错的**唯一**前提条件:集合已饱和且现有成员分数都高于 1。
	for i := 0; i < defaultOffendersTopN; i++ {
		incumbent := fmt.Sprintf("incumbent_%02d", i)
		for j := 0; j < 5; j++ {
			r.Record(class, incumbent)
		}
	}
	card, err := c.ZCard(key).Result()
	require.NoError(t, err)
	// 自证:前提必须真的成立。若填充阶段自己就被裁剪到不足 topN,
	// 下面的断言就不再检验"满集合"这个条件,测试会变成恒真。
	require.GreaterOrEqual(t, int(card), defaultOffendersTopN,
		"前提不成立:集合未达到 topN(%d),实际 %d —— 这条用例检验的是饱和状态下的行为",
		defaultOffendersTopN, card)

	// 现在来一个全新的 offender,只被拒了一次。
	const newcomer = "newcomer_bot"
	r.Record(class, newcomer)

	score, err := c.ZScore(key, newcomer).Result()
	require.NoError(t, err,
		"新 offender %q 在写入后立刻消失了——满集合把新来的饿死了。"+
			"影子模式将永远看不到「在既有 offender 之后才开始超限」的 bot,"+
			"而那正是定配额时最需要看见的一类。", newcomer)
	require.Equal(t, float64(1), score)

	// 它也必须能被 Top() 读到——只存在于 ZSet 里但读不出来同样等于看不见。
	top, err := r.Top(class, 0)
	require.NoError(t, err)
	var found bool
	for _, z := range top {
		if z.Member == newcomer {
			found = true
			break
		}
	}
	require.True(t, found,
		"新 offender 在 ZSet 里但 Top() 读不到——Top 的上界夹得比高水位还低,"+
			"排查时看不到水位区间内最新出现的 offender")
}

// TestOffenderRecorderIgnoresEmptyInput 确认防御性早返回:
// nil recorder / 空 key 不得 panic,也不得写出一个空成员污染名单。
func TestOffenderRecorderIgnoresEmptyInput(t *testing.T) {
	var nilRecorder *OffenderRecorder
	require.NotPanics(t, func() { nilRecorder.Record("business", "bot") })

	top, err := nilRecorder.Top("business", 10)
	require.NoError(t, err)
	require.Nil(t, top)

	r := NewOffenderRecorder(nil, "test:")
	require.NotPanics(t, func() { r.Record("business", "bot") })
}
