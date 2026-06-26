package avatarrender

import (
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件复现 issue#480 的真实根因:头像「按需渲染」在成员列表扇出(member-list
// fanout)下把 pod 的 CPU 打满,使同机的「邀请成员」请求 goroutine 被调度饿死。
//
// 复现不依赖 MySQL/Redis/WuKongIM —— 饿死发生在 Go 调度器层,与外部基础设施无关
// (这正是之前 SELECT 1 / ping / 网络探针都照不到、却把整请求三段等比拖慢的原因)。
// 用 GOMAXPROCS=2 模拟生产 2 核 pod;渲染洪峰 = avatarrender.Render 的并发循环;
// 受害者 = 模拟邀请 handler 的「~20 次串行 I/O 往返 + 每次唤醒后少量工作」。
//
// 受害者每次 I/O 往返用 time.Sleep(park) 模拟:I/O 返回后 goroutine 变为可运行,
// 但两个 P 都被渲染占满 → 唤醒要排队等 P。这一段排队 = 邀请慢的本质,且对
// SELECT 1 之类的独立进程探针不可见。

// victimWork 模拟一次「邀请成员」请求:ioRounds 次串行 I/O 往返,每次 park 后做
// 极少量 CPU 工作。理想耗时 ≈ ioRounds × ioRTT;被饿死时,park 后的唤醒等 P 的
// 时间会叠加进来,整体被放大。
func victimWork(ioRounds int, ioRTT time.Duration) time.Duration {
	start := time.Now()
	for i := 0; i < ioRounds; i++ {
		time.Sleep(ioRTT) // 一次 DB/Redis/WuKongIM 往返:goroutine park
		// I/O 返回后的少量工作(解析/拼装)。被饿死时,能跑到这里本身就被延迟了。
		sink := 0
		for j := 0; j < 2000; j++ {
			sink += j
		}
		_ = sink
	}
	return time.Since(start)
}

// medianVictim 连续测 n 次受害者耗时取中位数,降低单次抖动。
func medianVictim(n, ioRounds int, ioRTT time.Duration) time.Duration {
	ds := make([]time.Duration, n)
	for i := range ds {
		ds[i] = victimWork(ioRounds, ioRTT)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[n/2]
}

// startRenderFanout 启动 workers 个 goroutine 持续并发渲染头像,模拟成员列表扇出
// 下大量并发的 /users/{uid}/avatar 200 响应(无 If-None-Match,绕过 304 快路径,
// 每次真渲染)。返回停止函数,返回时已确保所有渲染 goroutine 退出。
func startRenderFanout(t *testing.T, workers int) (stop func(), rendered *int64) {
	t.Helper()
	var done int32
	var count int64
	var wg sync.WaitGroup
	seeds := []string{"u_alice", "u_bob", "u_carol", "u_dave", "u_erin"}
	texts := []string{"甲乙", "丙丁", "戊己", "庚辛", "壬癸"}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		seed := seeds[w%len(seeds)]
		text := texts[w%len(texts)]
		go func() {
			defer wg.Done()
			for atomic.LoadInt32(&done) == 0 {
				if _, err := Render(Options{Text: text, Bg: ColorForSeed(seed)}); err == nil {
					atomic.AddInt64(&count, 1)
				}
			}
		}()
	}
	return func() {
		atomic.StoreInt32(&done, 1)
		wg.Wait()
	}, &count
}

// TestRenderCost 量化单次渲染的 CPU 成本,坐实「渲染重」这一前提:
// 800×800 超采样 + CatmullRom 缩小 + PNG 编码。
func TestRenderCost(t *testing.T) {
	if testing.Short() {
		t.Skip("skip render cost in -short")
	}
	// 预热(字体一次性解析)。
	if _, err := Render(Options{Text: "甲乙", Bg: ColorForSeed("warm")}); err != nil {
		t.Fatalf("render: %v", err)
	}
	const n = 30
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := Render(Options{Text: "甲乙", Bg: ColorForSeed("seed")}); err != nil {
			t.Fatalf("render: %v", err)
		}
	}
	per := time.Since(start) / n
	t.Logf("单次 Render ≈ %v (n=%d, 串行)", per, n)
	if per < 1*time.Millisecond {
		t.Logf("注意:本机单次渲染异常快(%v),洪峰效应会相应减弱", per)
	}
}

// TestRenderFanoutStarvesVictim 是核心复现:GOMAXPROCS=2 下,渲染洪峰开启前后
// 对比「邀请」受害者的耗时。开启洪峰后受害者被显著放大 —— 这就是邀请变慢的根因,
// 且 DB/网络全程空闲。
func TestRenderFanoutStarvesVictim(t *testing.T) {
	if testing.Short() {
		t.Skip("skip starvation repro in -short")
	}
	prev := runtime.GOMAXPROCS(2) // 模拟生产 2 核 pod
	defer runtime.GOMAXPROCS(prev)

	const (
		ioRounds = 20                   // 邀请链路 ~14 DB + 3 redis + 3 wukongim
		ioRTT    = 1 * time.Millisecond // 单次往返(局域网级)
		samples  = 7
		workers  = 16 // 成员列表扇出的并发渲染数(远超 2 个 P)
	)

	// 1) 基线:无洪峰。
	base := medianVictim(samples, ioRounds, ioRTT)

	// 2) 洪峰下。
	stop, rendered := startRenderFanout(t, workers)
	// 让洪峰先把两个 P 占满再测量。
	time.Sleep(50 * time.Millisecond)
	under := medianVictim(samples, ioRounds, ioRTT)
	stop()

	ratio := float64(under) / float64(base)
	t.Logf("受害者(邀请模拟,理想 ≈ %v):", time.Duration(ioRounds)*ioRTT)
	t.Logf("  基线(无洪峰)         = %v", base)
	t.Logf("  渲染洪峰下           = %v", under)
	t.Logf("  放大倍数             = %.1fx", ratio)
	t.Logf("  洪峰期间共渲染头像   = %d 张 (workers=%d, GOMAXPROCS=2)", atomic.LoadInt64(rendered), workers)

	// 保守断言:被饿死时受害者至少放大 2x(实测通常远大于此;真实生产渲染更重、
	// 并发更高,放大到秒级)。阈值取保守值以免 CI 抖动误报。
	if ratio < 2.0 {
		t.Fatalf("期望渲染洪峰把受害者放大 ≥2x,实测 %.1fx(base=%v under=%v);"+
			"若本机渲染过快或核数受限,可调高 workers 重试", ratio, base, under)
	}
}

// TestCacheEliminatesStarvation 验证修复:同样的扇出,但渲染走真实的 avatarrender.Cache
// (LRU + singleflight + 渲染信号量)后,CPU 不再被反复渲染占满,受害者基本回到基线。
// 这测的是实际发布的修复代码,而非临时缓存桩。
func TestCacheEliminatesStarvation(t *testing.T) {
	if testing.Short() {
		t.Skip("skip cache validation in -short")
	}
	prev := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(prev)

	const (
		ioRounds = 20
		ioRTT    = 1 * time.Millisecond
		samples  = 7
		workers  = 16
	)

	base := medianVictim(samples, ioRounds, ioRTT)

	// 真实修复路径:头像经 Cache.GetOrRender,相同内容 key 命中复用字节、并发冷渲染
	// 由 singleflight 合并、渲染并发由信号量限制(模拟 2 核留 1 核给其它流量)。
	cache, err := NewCache(Config{MaxConcurrentRenders: 1})
	if err != nil {
		t.Fatal(err)
	}
	getCached := func(seed, text string) {
		key := text + "|" + seed // 与 ETag 同源的内容 key(此处简化)
		_, _ = cache.GetOrRender(key, func() ([]byte, error) {
			return Render(Options{Text: text, Bg: ColorForSeed(seed)})
		})
	}

	var done int32
	var served int64
	var wg sync.WaitGroup
	seeds := []string{"u_alice", "u_bob", "u_carol", "u_dave", "u_erin"}
	texts := []string{"甲乙", "丙丁", "戊己", "庚辛", "壬癸"}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		seed := seeds[w%len(seeds)]
		text := texts[w%len(texts)]
		go func() {
			defer wg.Done()
			for atomic.LoadInt32(&done) == 0 {
				getCached(seed, text)
				atomic.AddInt64(&served, 1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	under := medianVictim(samples, ioRounds, ioRTT)
	atomic.StoreInt32(&done, 1)
	wg.Wait()

	ratio := float64(under) / float64(base)
	t.Logf("经 avatarrender.Cache 的扇出:")
	t.Logf("  基线                 = %v", base)
	t.Logf("  缓存扇出下           = %v", under)
	t.Logf("  放大倍数             = %.1fx (期望接近 1x)", ratio)
	t.Logf("  缓存命中服务次数     = %d (仅首次每 key 渲染, 共 %d 个 key)", atomic.LoadInt64(&served), cache.Len())

	// 缓存后扇出几乎零 CPU,受害者不应被显著放大。给宽松上限避免抖动误报。
	if ratio > 1.8 {
		t.Fatalf("期望缓存消除饿死(放大接近 1x),实测 %.1fx(base=%v under=%v)", ratio, base, under)
	}
}
