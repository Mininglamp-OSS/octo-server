package searchetl

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// TestRunIncremental_KafkaOffIsLazy 惰性闸门：Kafka.On=false（默认）时 RunIncremental 立即返回 nil，
// **不连 Kafka、不读 DB**（在 cfg.Kafka.On 判定后直接返回，不触达 dbNowUnix / producer 构造）。
// 这保证阶段 2 合并部署到测试环境后无运行期行为变更，须显式开 flag 才跑（"先测试验证再上生产"）。
func TestRunIncremental_KafkaOffIsLazy(t *testing.T) {
	cfg := config.New()
	// 显式确认默认关闭（阶段 1 已设默认；此处兜底防回归）。
	cfg.Kafka.On = false
	ctx := config.NewContext(cfg)
	etl := NewETL(ctx)

	// 若惰性闸门失效，会进而调用 dbNowUnix（无 DB 连接将报错/超时）；返回 nil 即证明短路。
	if err := etl.RunIncremental(context.Background()); err != nil {
		t.Fatalf("Kafka.On=false must short-circuit to nil, got err=%v", err)
	}
}

// TestRunIncremental_ReentrancyGuard 进程内重入护栏：已有一轮在跑（running=true）时，
// 即便 Kafka.On=true，RunIncremental 也立即跳过返回 nil——不构造 producer、不连 Kafka、
// 不读 DB。证明同进程并发不会对同批源行重复投递（跨副本互斥仍由阶段 3 Redis 锁负责）。
func TestRunIncremental_ReentrancyGuard(t *testing.T) {
	cfg := config.New()
	cfg.Kafka.On = true // 即便开了 Kafka，重入护栏也应先于 producer 构造短路
	cfg.Kafka.Brokers = []string{"127.0.0.1:0"}
	ctx := config.NewContext(cfg)
	etl := NewETL(ctx)

	// 模拟「已有一轮在跑」。
	if !etl.running.CompareAndSwap(false, true) {
		t.Fatalf("precondition: running should start false")
	}
	defer etl.running.Store(false)

	// 第二轮必须被护栏挡下、立即返回 nil（不触达 newProducer / dbNowUnix）。
	if err := etl.RunIncremental(context.Background()); err != nil {
		t.Fatalf("reentrant run must short-circuit to nil, got err=%v", err)
	}
}
