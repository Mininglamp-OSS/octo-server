package avatarrender

import (
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
)

// 头像「按需渲染」在成员列表扇出下会把 pod 的 CPU 打满,饿死同机的其它请求
// goroutine(见 issue#480)。Cache 用三层防御把"一次扇出 N 张 = N 次渲染"压成
// 最多一次:
//
//   - bytes LRU:渲染结果按内容 key 缓存,命中直接复用字节,零 CPU;
//   - singleflight:同 key 的并发请求只触发一次渲染,其余复用同一结果(防冷启动
//     惊群——首屏 N 个并发请求打到同一张未缓存头像时只渲一次);
//   - render 信号量:限制全局并发渲染数,即使大量"不同 key"的冷头像同时到达,也不
//     会占满所有 P 把其它流量饿死(信号量必须配合前两层,否则只是把惊群变成慢排队)。
//
// key 由调用方按"决定图像内容的全部因子"生成(与 ETag 同源),Cache 不关心其语义。

// DefaultCacheSize 是未配置时的 LRU 容量(条目数)。
//
// 注意:LRU 按**条目数**而非字节计 bound。当前所有渲染输出都是 200×200 的简单 PNG
// (数 KB 量级),故 4096 条目 ≈ 十几 MB,足够覆盖活跃用户集。若将来有调用方(如
// #478 群头像)缓存尺寸差异很大的图,常驻内存会随 entries×单图尺寸增长,届时应改为
// 按字节预算的缓存或下调容量(PR#481 评审)。
const DefaultCacheSize = 4096

// Hooks 是可观测性回调,全部可选(nil 即 no-op)。刻意用函数字段而非接口,使本包
// 不依赖 pkg/metrics —— 由上层(modules/user)在构造时把 prometheus 观测点接进来。
type Hooks struct {
	// OnHit / OnMiss:LRU 命中 / 未命中各记一次。
	OnHit  func()
	OnMiss func()
	// OnShared:本次渲染结果被并发调用方复用(singleflight 合并),记一次。
	OnShared func()
	// OnRender:一次真实渲染完成,带耗时与错误(用于渲染延迟直方图)。
	OnRender func(dur time.Duration, err error)
	// OnSemaphoreWait:获取渲染信号量的等待耗时(>0 说明渲染并发已饱和、在排队)。
	OnSemaphoreWait func(dur time.Duration)
	// OnInflight:正在进行的渲染数变化(+1 开始 / -1 结束),用于 inflight gauge。
	OnInflight func(delta float64)
}

func (h *Hooks) normalize() {
	if h.OnHit == nil {
		h.OnHit = func() {}
	}
	if h.OnMiss == nil {
		h.OnMiss = func() {}
	}
	if h.OnShared == nil {
		h.OnShared = func() {}
	}
	if h.OnRender == nil {
		h.OnRender = func(time.Duration, error) {}
	}
	if h.OnSemaphoreWait == nil {
		h.OnSemaphoreWait = func(time.Duration) {}
	}
	if h.OnInflight == nil {
		h.OnInflight = func(float64) {}
	}
}

// Config 配置 Cache。
type Config struct {
	// Size 是 LRU 最大条目数;<=0 用 DefaultCacheSize。
	Size int
	// MaxConcurrentRenders 是同时进行的真实渲染数上限(信号量容量);<=0 表示不限。
	// 取值应 < GOMAXPROCS,给非头像流量留核;配合缓存后,渲染只在冷 key 时发生,
	// 串行化冷渲染的代价是一次性的(之后命中缓存),换来洪峰下不饿死其它请求。
	MaxConcurrentRenders int
	// Hooks 是可观测性回调。
	Hooks Hooks
}

// Cache 是带 singleflight 与渲染信号量的头像 bytes 缓存。零值不可用,须经 NewCache 构造;
// 方法对 nil 接收者安全(直接渲染,无缓存),便于在未配置缓存时退化。
type Cache struct {
	lru   *lru.Cache[string, []byte]
	group singleflight.Group
	sem   chan struct{}
	hooks Hooks
}

// NewCache 构造一个 Cache。
func NewCache(cfg Config) (*Cache, error) {
	size := cfg.Size
	if size <= 0 {
		size = DefaultCacheSize
	}
	l, err := lru.New[string, []byte](size)
	if err != nil {
		return nil, err
	}
	cfg.Hooks.normalize()
	c := &Cache{lru: l, hooks: cfg.Hooks}
	if cfg.MaxConcurrentRenders > 0 {
		c.sem = make(chan struct{}, cfg.MaxConcurrentRenders)
	}
	return c, nil
}

// GetOrRender 返回 key 对应的头像字节:命中 LRU 直接返回;否则经 singleflight 合并、
// 在渲染信号量约束下调用 render 生成一次,成功后写入 LRU。render 必须是确定性的
// (相同 key → 相同字节),否则缓存会返回过期内容。
//
// nil 接收者时直接调用 render(无缓存/合并/限流),用于缓存未配置的退化路径。
func (c *Cache) GetOrRender(key string, render func() ([]byte, error)) ([]byte, error) {
	if c == nil {
		return render()
	}
	if v, ok := c.lru.Get(key); ok {
		c.hooks.OnHit()
		return v, nil
	}
	c.hooks.OnMiss()

	// ranRender 标记本次调用是否真的执行了渲染。singleflight 只会运行 leader 的闭包,
	// 故只有 leader 这里被置 true;等待者的闭包不运行,保持 false。各 goroutine 持有
	// 自己的局部 ranRender,无跨协程访问,无需同步。
	ranRender := false
	v, err, shared := c.group.Do(key, func() (interface{}, error) {
		// 双检:在等待 singleflight 期间,可能已有同 key 的先行渲染填好了缓存。
		if v, ok := c.lru.Get(key); ok {
			return v, nil
		}
		ranRender = true
		b, rerr := c.renderWithSem(render)
		if rerr != nil {
			return nil, rerr
		}
		c.lru.Add(key, b)
		return b, nil
	})
	// singleflight 的 shared 对 leader 也为 true。OnShared 只应计"被合并到他人渲染
	// 上的请求"(真正搭便车的等待者),故排除自己执行了渲染的 leader,避免每次 flight
	// 多计一次(PR#481 评审)。
	if shared && !ranRender {
		c.hooks.OnShared()
	}
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// renderWithSem 在渲染信号量(若配置)约束下执行一次渲染,并打点等待/耗时/inflight。
func (c *Cache) renderWithSem(render func() ([]byte, error)) (b []byte, err error) {
	if c.sem != nil {
		start := time.Now()
		c.sem <- struct{}{}
		// 先注册释放,再打点等待耗时:即使 OnSemaphoreWait hook panic,已获取的令牌
		// 也必被归还,不会泄漏导致信号量逐渐枯竭/死锁(PR#481 评审,防御性)。
		defer func() { <-c.sem }()
		c.hooks.OnSemaphoreWait(time.Since(start))
	}
	c.hooks.OnInflight(1)
	defer c.hooks.OnInflight(-1)

	// 用 defer 记录渲染耗时/结果,这样 render() panic 时指标也能落点(否则 Prometheus
	// 看不到系统性渲染崩溃)。捕获后原样重抛,保持 panic 传播语义不变(PR#481 评审 P2)。
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			c.hooks.OnRender(time.Since(start), fmt.Errorf("avatarrender: render panicked: %v", r))
			panic(r)
		}
		c.hooks.OnRender(time.Since(start), err)
	}()
	return render()
}

// Len 返回当前缓存的条目数(测试/诊断用)。nil 接收者返回 0。
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	return c.lru.Len()
}
