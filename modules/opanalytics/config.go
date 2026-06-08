package opanalytics

import (
	"os"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// 报告时区为**部署级**配置(国内/海外分开部署、独立库)：国内默认东八，海外配当地。
// 做成 config 不硬编码；天粒度，不上小时桶。message.timestamp 是绝对纪元秒，
// 报告时区只在 ETL 日切分桶 / handler 解析 start_date~end_date 时应用。
const (
	envReportTimezone     = "DM_OPANALYTICS_TIMEZONE"
	defaultReportTimezone = "Asia/Shanghai"
)

var (
	_reportLoc  *time.Location
	_reportOnce sync.Once
)

// reportLocation 返回部署级报告时区。读取 DM_OPANALYTICS_TIMEZONE(IANA 名)，
// 缺省东八；解析失败时告警并回退东八，永不 panic。
func reportLocation() *time.Location {
	_reportOnce.Do(func() {
		name := os.Getenv(envReportTimezone)
		if name == "" {
			name = defaultReportTimezone
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			log.Warn("invalid DM_OPANALYTICS_TIMEZONE, falling back to default",
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
