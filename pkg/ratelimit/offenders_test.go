package ratelimit

import (
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

	card, err := c.ZCard(key).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, int(card), defaultOffendersTopN,
		"灌入 %d 个成员后集合应被裁剪到 ≤%d,实际 %d —— ZREMRANGEBYRANK 未生效,集合无界",
		inserted, defaultOffendersTopN, card)

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
