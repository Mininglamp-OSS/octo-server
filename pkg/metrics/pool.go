package metrics

import (
	"database/sql"

	rd "github.com/go-redis/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// 连接池指标。两类依赖的连接池都用 scrape 时读取的 Collector 暴露,
// 不起后台采样 goroutine —— 抓取时刻即时读 .Stats()/.PoolStats(),数据天然新鲜,
// 也没有 goroutine 生命周期 / 泄漏问题。
//
// 连接池饱和(等待连接、等待超时)是请求秒级长尾的经典根因,这些指标用于在
// dashboard 上直接判断长尾是否来自池耗尽。

// RegisterPoolCollectors 把 DB 与 Redis 连接池 Collector 注册到 reg。
//
//   - DB 复用 client_golang 自带的 DBStatsCollector,暴露标准 go_sql_* 序列
//     (open/in_use/idle/wait_count/wait_duration_seconds_total),社区 Grafana
//     dashboard 开箱即用。db 为 nil 时跳过。
//   - Redis 用下方自定义 Collector 读 *redis.Client.PoolStats()(lib 的
//     GetRedisConn() 返回 *redis.Conn 不暴露 PoolStats,够不到,不在此列)。
//     clients 为空时跳过。
//
// 注册契约与其它指标一致:同一 reg 只应调用一次,重复注册触发 MustRegister 的
// panic(prometheus 库契约)。
func RegisterPoolCollectors(reg prometheus.Registerer, db *sql.DB, redisClients map[string]*rd.Client) {
	if db != nil {
		// "main" 是单库现状下的固定 db name label。若将来引入读副本/第二连接池,
		// 这里需参数化以免重复注册冲突(#442 P2-2,当前单库无需处理)。
		reg.MustRegister(collectors.NewDBStatsCollector(db, "main"))
	}
	if len(redisClients) > 0 {
		reg.MustRegister(newRedisPoolCollector(redisClients))
	}
}

// redisPoolCollector 在每次 scrape 时读取一组 *redis.Client 的 PoolStats,
// 按 client label 区分多个连接池。实现 prometheus.Collector。
type redisPoolCollector struct {
	clients map[string]*rd.Client

	totalConns *prometheus.Desc
	idleConns  *prometheus.Desc
	hits       *prometheus.Desc
	misses     *prometheus.Desc
	timeouts   *prometheus.Desc
	staleConns *prometheus.Desc
}

func newRedisPoolCollector(clients map[string]*rd.Client) *redisPoolCollector {
	const ns, sub = metricNamespace, "redis_pool"
	labels := []string{"client"}
	// 复制一份调用方传入的 map,避免后续调用方动态增删该 map 时影响 Collect
	// 读取的 client 集合(Jerry-Xin #442 review)。
	owned := make(map[string]*rd.Client, len(clients))
	for name, c := range clients {
		owned[name] = c
	}
	return &redisPoolCollector{
		clients: owned,
		totalConns: prometheus.NewDesc(prometheus.BuildFQName(ns, sub, "total_connections"),
			"Current total number of connections in the redis pool.", labels, nil),
		idleConns: prometheus.NewDesc(prometheus.BuildFQName(ns, sub, "idle_connections"),
			"Current number of idle connections in the redis pool.", labels, nil),
		hits: prometheus.NewDesc(prometheus.BuildFQName(ns, sub, "hits_total"),
			"Cumulative number of times a free connection was found in the pool.", labels, nil),
		misses: prometheus.NewDesc(prometheus.BuildFQName(ns, sub, "misses_total"),
			"Cumulative number of times a free connection was NOT found in the pool.", labels, nil),
		timeouts: prometheus.NewDesc(prometheus.BuildFQName(ns, sub, "timeouts_total"),
			"Cumulative number of times a wait timeout occurred (pool saturated).", labels, nil),
		staleConns: prometheus.NewDesc(prometheus.BuildFQName(ns, sub, "stale_connections_total"),
			"Cumulative number of stale connections removed from the pool.", labels, nil),
	}
}

// Describe 发送全部 desc。实现 prometheus.Collector。
func (c *redisPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalConns
	ch <- c.idleConns
	ch <- c.hits
	ch <- c.misses
	ch <- c.timeouts
	ch <- c.staleConns
}

// Collect 在 scrape 时读取每个 client 的 PoolStats 并发出即时样本。
// 实现 prometheus.Collector。
//
// Counter / Gauge 分类依据 go-redis v6 PoolStats 字段语义:
//   - Counter(累积值):Hits / Misses / Timeouts / StaleConns —— 自启动以来的次数;
//   - Gauge(瞬时快照):TotalConns / IdleConns —— 当前池内连接数。
func (c *redisPoolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, client := range c.clients {
		if client == nil {
			continue
		}
		s := client.PoolStats()
		if s == nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.totalConns, prometheus.GaugeValue, float64(s.TotalConns), name)
		ch <- prometheus.MustNewConstMetric(c.idleConns, prometheus.GaugeValue, float64(s.IdleConns), name)
		ch <- prometheus.MustNewConstMetric(c.hits, prometheus.CounterValue, float64(s.Hits), name)
		ch <- prometheus.MustNewConstMetric(c.misses, prometheus.CounterValue, float64(s.Misses), name)
		ch <- prometheus.MustNewConstMetric(c.timeouts, prometheus.CounterValue, float64(s.Timeouts), name)
		ch <- prometheus.MustNewConstMetric(c.staleConns, prometheus.CounterValue, float64(s.StaleConns), name)
	}
}
