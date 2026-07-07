# perf-todo — sticker-upload-compression

灰度前必须完成的性能验证清单。**本任务当前状态：功能就绪、性能未验证**。
在 `sticker.compress_enabled=true` 打开到任何生产流量之前，必须回答本清单
的每一项，把结果贴在本文件下方对应小节，作为放量决策依据。

## 0. 已有的观测能力

- Counter `sticker_upload_total{result}` — 新增 4 个 compress_* 维度已预热为 0
- Histogram `sticker_compress_duration_seconds{outcome,format}` — 仅在
  acquire slot 之后打点（compressed / over_limit / failed / skipped:timeout），
  disabled/format/concurrency_saturated 分支耗时约等于 0，用 counter 观测即可

Grafana 上线灰度前必须准备的面板：
- P50/P95/P99 `sticker_compress_duration_seconds` 按 outcome、format 分维度
- `rate(sticker_upload_total{result=~"compress_.*"}[5m])` 分四维度堆叠图
- `histogram_quantile(0.99, ...)` 与配置 `compress_timeout_ms` 的对比线
- 进程 CPU / goroutine 数 / heap alloc 曲线（对齐灰度切换时间点）

## 1. 单机 CPU 基线（本地 bench，DONE — Apple M5）

```
go test -run x -bench='BenchmarkDoCompressStaticSticker' -benchmem -benchtime=3s ./modules/file/
```

| Benchmark                                     | ns/op       | ~ms/op | alloc/op |
| --------------------------------------------- | ----------- | ------ | -------- |
| JPEG 512² re-encode                           | 12,046,858  | ~12    | 936 KB   |
| PNG 512²  re-encode                           | 29,883,139  | ~30    | 4.5 MB   |
| JPEG 1024² → Fit(512) + re-encode             | 34,451,231  | ~34    | 5.6 MB   |
| PNG  1024² → Fit(512) + re-encode             | 51,525,786  | ~52    | 11.2 MB  |
| JPEG 1024² re-encode only (maxDim=1024)       | 49,102,169  | ~49    | 3.7 MB   |
| PNG  1024² re-encode only (maxDim=1024)       | 111,301,979 | ~111   | 15.6 MB  |

**关键推论**（M5，生产 x86 云 VM 通常慢 2-3×，用 3× 保守估算）：

- **最坏单帧耗时**：PNG 1024² re-encode ≈ **~330 ms**（生产估）
- **最坏单帧内存 alloc**：~15 MB / 帧 → concurrency=4 时峰值 **~60 MB**（可控）
- **QPS 上限**（生产估）：
  - concurrency=4：JPEG 512² ~110 QPS；PNG 1024² ~12 QPS
  - concurrency=8：JPEG 512² ~220 QPS；PNG 1024² ~24 QPS

**默认值合理性初判**：
- `compress_max_concurrency=4` — 单实例上限保守，防止 CPU 打满。真实
  贴纸 <1024² + JPEG 占多数，实测 QPS 天花板应远高于业务贴纸创建速率。
- `compress_timeout_ms=2000` — 相对 ~330 ms 最坏单帧有 **6× 余量**，能吸收
  抖动。若生产观测到 P99 < 500 ms 稳定，可下调到 1000 ms 提升拒绝速度。

## 2. 待补的性能验证（灰度前 MUST）

### 2.1 生产 CPU 型号下的 bench 复跑

在实际生产 Node（x86 云 VM，非 M5）上跑上面这条 `go test -bench=...` 拿到
本地基线的 3× / 4× 因子的实测校正值。**若结果偏离本地估算 ±50%，需重新
评估 concurrency / timeout 默认值**。

- [ ] 生产型号 CPU 上跑一遍 bench，把六行结果贴到本文件
- [ ] 若 PNG 1024² 单帧 > 1000 ms，把 `compress_timeout_ms` 默认调高到
      3000 ms 再放量，否则频繁 skip:timeout 会让压缩形同虚设

### 2.2 pprof CPU / heap 剖面

- [ ] `go test -run x -bench='BenchmarkDoCompressStaticSticker_PNG_1024' -cpuprofile=cpu.out`
  → `go tool pprof -top cpu.out`，确认 CPU 时间主要在 imaging.Encode / Lanczos，
  没有奇怪的框架开销
- [ ] `-memprofile=mem.out`：确认 alloc/op 与 bench 报的一致；无异常泄漏

### 2.3 端到端 HTTP 压测

用 `hey` 或 `wrk` 打 `POST /v1/file/upload?type=sticker`（真实 auth token +
multipart 载体），采集：

- [ ] 稳态 QPS = 目标生产 QPS × 1.5 时的 P50/P95/P99 延迟
- [ ] 稳态 CPU 使用率（应 < 70%）
- [ ] 稳态 heap alloc 曲线（应平稳、无线性上涨 = 无泄漏）
- [ ] 稳态 goroutine 数（应有界、无线性上涨 = 无 dangling worker）
- [ ] 错误分布：`compress_over_limit` / `compress_skipped:timeout` /
      `compress_failed` 各占比

**过关标准（建议）**：
- P99 < timeout 值 × 0.8（有余量，避免 timeout 密集触发）
- `compress_skipped:concurrency_saturated` < 1%（并发数够用）
- `compress_skipped:timeout` < 0.1%（timeout 足够）
- CPU steady < 70%

### 2.4 Goroutine 泄漏边界测试

**风险点**：`stickerCompressor.Compress` 走 timeout 分支时，后台 goroutine 未
被取消，仍在跑到 `doCompress` 自然结束（imaging 无 context）才退出。在
"客户端持续上传导致每次都 timeout"的极端场景下，dangling goroutine 会累积。

单帧 dangling 时间 = doCompress 完整耗时 - timeoutMs（PNG 1024² 最坏 ~110 ms
超出 2000 ms 的场景需要更大的图，不常见）。**但需要显式压测**：

- [ ] 用故意注入的 slow `doCompress`（sleep 5s），以 timeout=100 ms 打 1000 QPS
      持续 60s，观察 goroutine 峰值 —— 应 ≤ `concurrency × ceil(5s/100ms) = 200`，
      不应无限增长（compressor sem 已卡上限）
- [ ] 若观察到线性增长，说明 sem 语义有 bug 或 goroutine 未按预期退出

### 2.5 Mutex 争用评估

**风险点**：`stickerCompressor.mu` 是全局单锁，`tryAcquireCompressSlot` /
`releaseCompressSlot` 每次请求进出各锁一次。高 QPS 下可能成为热点。

- [ ] `go test -run x -bench='...' -blockprofile=block.out` → `go tool pprof
      -top block.out`，看 mu 是否进入 top-3
- [ ] 若是热点，考虑改 `atomic.Int32` + CAS（现在的 mutex 语义"数据比较"完全
      可用原子操作代替，无 map/slice 保护需求）

**当前判断**：默认 concurrency=4 时，稳态最多 4 个持锁 goroutine + 后来者一次
tryAcquire 就 return，锁窗口 ~ns 级，不会是热点。但配合 §2.3 压测数据佐证。

### 2.6 Decompression bomb 防御

维度上限 1024²（配置硬 cap）→ 解码后 RGBA 内存 = 1024×1024×4 = **4 MB / 帧**。
concurrency=4 时峰值 16 MB，安全。

- [ ] 灰度前跑一次已知恶意样本（1 KB 压缩包解出 10000² 位图），确认在
      `image.DecodeConfig` 阶段被 dimension 校验拦住，**不进入压缩管线**
      （压缩管线的 image.Decode 走 full-decode 才是攻击面，前置维度门是
      唯一防线）

## 3. 灰度切换的操作剧本

- [ ] Step 1: 打开 `sticker.compress_enabled=true` 到 **单实例 5% 流量**
      （或用 GrayScale 平台把其中 1 台 pod 单独打标签）
- [ ] Step 2: 观察 15 分钟：`compress_success` 占比 > 50%（说明格式命中）、
      P99 < 500 ms、CPU 增幅 < 10%
- [ ] Step 3: 逐步放量到 50% → 100%，每步观察 30 分钟
- [ ] Step 4: 生产观测稳定 24h 后再调 `compress_max_concurrency` / `_timeout_ms`
      默认值到实测最优（下调 timeout 提升拒绝速度、按 CPU 富余上调 concurrency）

## 4. 回滚剧本

- 一键关：`sticker.compress_enabled=false`（60s 内 all replica 收敛）
- 缩容闸：`sticker.compress_max_concurrency=1` 快速降低 CPU 占用（不需要
  完全关，只是缩容压力）
- 兜底：所有压缩失败路径都 fail-open 走原字节，即使 compressor 完全瘫痪
  上传主链路不受影响（brief 的 acceptance 已保证）
