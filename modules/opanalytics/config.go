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
