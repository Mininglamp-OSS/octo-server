package opanalytics

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// 报告时区为**部署级**配置(国内/海外分开部署、独立库)：国内默认东八，海外配当地。
// 做成 config 不硬编码；天粒度，不上小时桶。message.timestamp 是绝对纪元秒，
// 报告时区只在 ETL 日切分桶 / handler 解析 start_date~end_date 时应用。
const (
	envReportTimezone     = "OCTO_OPANALYTICS_TIMEZONE"
	defaultReportTimezone = "Asia/Shanghai"

	// envETLBatch 单次 keyset 分页从 message 分片抽取的行数上限。增量抽取按
	// `WHERE id>cursor ORDER BY id LIMIT batch` 流式读取，batch 同时界定单 chunk
	// 的内存与持锁时长；过大增加事务/锁压力，过小增加往返。
	envETLBatch     = "OCTO_OPANALYTICS_ETL_BATCH"
	defaultETLBatch = 5000
	minETLBatch     = 100
	maxETLBatch     = 50000

	// envETLLagSeconds 抽取稳定性滞后窗口(秒)。message.id 在 INSERT 时分配、COMMIT 时
	// 才可见，而提交顺序≠id 顺序：低 id 的事务可能晚于高 id 提交。若严格按 id>cursor 推进，
	// 会漏掉"已被游标越过、之后才提交"的低 id 行。对策：只处理 created_at ≤ DB_NOW-lag 的
	// 已稳定前缀(id 与 created_at 同在 insert 时刻分配、近似同序，故稳定行构成无空洞前缀)，
	// 游标只推进到该前缀末尾。要求 lag > 单条消息落库事务的最大时长(消息热路径写入近乎即时
	// 提交，默认 10min 远超之)。设 0 关闭(仅测试/单实例可控场景)。
	envETLLagSeconds     = "OCTO_OPANALYTICS_ETL_LAG_SECONDS"
	defaultETLLagSeconds = 600
	maxETLLagSeconds     = 86400
)

var (
	_reportLoc  *time.Location
	_reportOnce sync.Once
)

// reportLocation 返回部署级报告时区。读取 OCTO_OPANALYTICS_TIMEZONE(IANA 名)，
// 缺省东八；解析失败时告警并回退东八，永不 panic。
func reportLocation() *time.Location {
	_reportOnce.Do(func() {
		name := os.Getenv(envReportTimezone)
		if name == "" {
			name = defaultReportTimezone
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			log.Warn("invalid OCTO_OPANALYTICS_TIMEZONE, falling back to default",
				zap.String("value", name), zap.String("fallback", defaultReportTimezone), zap.Error(err))
			loc, err = time.LoadLocation(defaultReportTimezone)
			if err != nil {
				loc = time.FixedZone("CST", 8*3600)
			}
		}
		_reportLoc = loc
	})
	return _reportLoc
}

// etlBatchSize 返回增量抽取的分页大小(读 OCTO_OPANALYTICS_ETL_BATCH，钳制到 [min,max])。
func etlBatchSize() int {
	v := os.Getenv(envETLBatch)
	if v == "" {
		return defaultETLBatch
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minETLBatch {
		if err != nil {
			log.Warn("invalid OCTO_OPANALYTICS_ETL_BATCH, using default",
				zap.String("value", v), zap.Int("default", defaultETLBatch), zap.Error(err))
		}
		if n < minETLBatch {
			return minETLBatch
		}
		return defaultETLBatch
	}
	if n > maxETLBatch {
		return maxETLBatch
	}
	return n
}

// etlLagSeconds 返回抽取稳定性滞后窗口秒数(读 OCTO_OPANALYTICS_ETL_LAG_SECONDS，
// 钳制到 [0,max])。解析失败回退默认值。
func etlLagSeconds() int64 {
	v := os.Getenv(envETLLagSeconds)
	if v == "" {
		return defaultETLLagSeconds
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Warn("invalid OCTO_OPANALYTICS_ETL_LAG_SECONDS, using default",
			zap.String("value", v), zap.Int64("default", defaultETLLagSeconds), zap.Error(err))
		return defaultETLLagSeconds
	}
	if n < 0 {
		return 0
	}
	if n > maxETLLagSeconds {
		return maxETLLagSeconds
	}
	return n
}
