package metrics

import (
	"context"
	"time"
)

// DependencyPing 是合成探针的 dependency label 值。探针周期性 Ping 上游
// (MySQL / Redis),把「取连接 + 一次往返」的耗时打进与业务调用同一个
// dependency 直方图(octo_server_dependency_duration_seconds),无需新增指标族。
//
// 背景(连接层过路费排查):线上观测到 health 的 db.Ping 偶发卡 ~2s,而真正
// 的 SQL 执行很快 —— 慢的是「取/建连接 + RTT」这一跳。把它做成常开 SLI,就能
// 持续量这笔过路费、设告警,并与慢请求时间线对照,无需逐调用点埋日志。
const DependencyPing = "ping"

// ObservePing 记录一次合成探针调用。backend 为低基数枚举(如 mysql /
// redis_ratelimit),op 固定为 "probe"。未初始化默认 DependencyMetrics 时为
// no-op(指标关闭 / 单测未初始化),绝不 panic —— 与 ObserveObjectStore 同语义。
func ObservePing(backend string, start time.Time, err error) {
	if m := defaultDependencyMetrics.Load(); m != nil {
		m.Observe(DependencyPing, "probe", backend, start, err)
	}
}

// pingTarget 是一个被周期性探测的上游。ping 接收带超时的 context,返回 error
// 即视为一次失败探测(status=error),但循环本身不因单次失败中断。
type pingTarget struct {
	backend string
	ping    func(context.Context) error
}

// PingProber 周期性对一组上游做 Ping,并把每次耗时灌进 ObservePing。
// 它只产生指标、不打日志;Start 起一个遵守 context 取消的后台 goroutine。
//
// 设计取舍:
//   - 复用业务依赖直方图(加 dependency="ping" label),不新增指标族,符合
//     pkg/metrics「新依赖加 label 值即可」的约定。
//   - 每次探测套独立超时,避免某个上游卡死拖垮整个探测循环 / 泄漏 goroutine。
//   - interval 取相对稀疏(默认调用方传 15s):过路费是分钟级的间歇抖动,过密
//     探测只会徒增连接开销而不提升信噪比。
type PingProber struct {
	interval time.Duration
	timeout  time.Duration
	targets  []pingTarget
}

// NewPingProber 构造一个探针。interval 是相邻探测轮的间隔,timeout 是单次探测
// 的上限(应 < interval)。两者非正值时回退到安全默认(15s / 3s)。
func NewPingProber(interval, timeout time.Duration) *PingProber {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &PingProber{interval: interval, timeout: timeout}
}

// Add 注册一个待探测上游。backend 必须是低基数枚举值(进 Prometheus label)。
// 返回自身以便链式调用。
func (p *PingProber) Add(backend string, ping func(context.Context) error) *PingProber {
	if ping == nil || backend == "" {
		return p
	}
	p.targets = append(p.targets, pingTarget{backend: backend, ping: ping})
	return p
}

// Start 在后台起探测循环,随 ctx 取消而退出。无 target 时不起 goroutine。
func (p *PingProber) Start(ctx context.Context) {
	if len(p.targets) == 0 {
		return
	}
	go p.loop(ctx)
}

func (p *PingProber) loop(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	// 启动即探一次,不必等满一个 interval 才有数据。
	p.probeAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeAll(ctx)
		}
	}
}

// probeAll 串行探测全部 target 并各记一次耗时。导出供测试直接驱动一轮探测,
// 无需起 goroutine / 等 ticker。
func (p *PingProber) probeAll(ctx context.Context) {
	for _, t := range p.targets {
		probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
		start := time.Now()
		err := t.ping(probeCtx)
		cancel()
		ObservePing(t.backend, start, err)
	}
}
