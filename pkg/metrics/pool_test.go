package metrics

import (
	"strings"
	"testing"

	rd "github.com/go-redis/redis"
	"github.com/prometheus/client_golang/prometheus"
)

// newTestRedisClient 构造一个指向回环地址的 client。不需要真实 Redis:PoolStats()
// 读的是客户端侧连接池计数器,即便没有可用服务端也返回有效(多为 0)的快照。
func newTestRedisClient() *rd.Client {
	return rd.NewClient(&rd.Options{Addr: "127.0.0.1:0"})
}

func TestRedisPoolCollector_EmitsSeries(t *testing.T) {
	client := newTestRedisClient()
	defer client.Close()

	reg := prometheus.NewRegistry()
	RegisterPoolCollectors(reg, nil, map[string]*rd.Client{"ratelimit": client})

	// 六个序列(total/idle gauge + hits/misses/timeouts/stale counter)均应出现,
	// 且带 client="ratelimit" label。
	wantNames := []string{
		"dmwork_redis_pool_total_connections",
		"dmwork_redis_pool_idle_connections",
		"dmwork_redis_pool_hits_total",
		"dmwork_redis_pool_misses_total",
		"dmwork_redis_pool_timeouts_total",
		"dmwork_redis_pool_stale_connections_total",
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		got[mf.GetName()] = true
		for _, metric := range mf.GetMetric() {
			var hasClient bool
			for _, l := range metric.GetLabel() {
				if l.GetName() == "client" && l.GetValue() == "ratelimit" {
					hasClient = true
				}
			}
			if !hasClient {
				t.Errorf("%s missing client=ratelimit label", mf.GetName())
			}
		}
	}
	for _, n := range wantNames {
		if !got[n] {
			t.Errorf("missing metric family %q", n)
		}
	}
}

func TestRedisPoolCollector_SkipsNilClient(t *testing.T) {
	reg := prometheus.NewRegistry()
	// nil client 不应导致 Collect panic,直接跳过。
	RegisterPoolCollectors(reg, nil, map[string]*rd.Client{"dead": nil})
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather with nil client: %v", err)
	}
}

func TestRegisterPoolCollectors_NoInputsNoPanic(t *testing.T) {
	reg := prometheus.NewRegistry()
	// db=nil 且无 redis client:不注册任何 collector,不 panic。
	RegisterPoolCollectors(reg, nil, nil)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "dmwork_redis_pool") || strings.HasPrefix(mf.GetName(), "go_sql") {
			t.Errorf("unexpected family %q registered with no inputs", mf.GetName())
		}
	}
}
