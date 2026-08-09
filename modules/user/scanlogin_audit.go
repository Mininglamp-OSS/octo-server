package user

import "go.uber.org/zap"

type scanLoginWarningLogger interface {
	Warn(message string, fields ...zap.Field)
}

type scanLoginRedemptionAudit struct {
	logger     scanLoginWarningLogger
	uuid       string
	scannerUID string
	completed  bool
}

func newScanLoginRedemptionAudit(
	logger scanLoginWarningLogger,
	uuid string,
	scannerUID string,
) *scanLoginRedemptionAudit {
	return &scanLoginRedemptionAudit{
		logger:     logger,
		uuid:       uuid,
		scannerUID: scannerUID,
	}
}

func (a *scanLoginRedemptionAudit) MarkCompleted() {
	if a != nil {
		a.completed = true
	}
}

func (a *scanLoginRedemptionAudit) WarnIfIncomplete() {
	if a == nil || a.completed || a.logger == nil {
		return
	}
	a.logger.Warn(
		"扫码授权已消费但登录未完成",
		zap.String("uuid", a.uuid),
		zap.String("scanner_uid", a.scannerUID),
	)
}
