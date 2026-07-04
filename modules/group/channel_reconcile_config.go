package group

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ChannelReconcileConfig 孤儿群 IM 频道对账 worker 的运行参数。#394
type ChannelReconcileConfig struct {
	// Enabled 总开关。默认 true——这是数据一致性兜底，正常运行下只是每 Interval 跑一次
	// 廉价的空查询（无 channel_synced=0 行时零动作），默认开启才能让 #394 修复开箱生效。
	Enabled bool
	// Interval 两次扫描之间的间隔。<=0 回退默认值。
	Interval time.Duration
	// Grace 宽限期：只处理 created_at 早于 now-Grace 的未同步群，避免与仍在途的建群主
	// 路径抢跑（建群 commit 后尚未标记 synced 的极短窗口）。<0 回退默认值。
	Grace time.Duration
	// BatchSize 单轮最多处理的孤儿群数，给 IM/DB 限流。<=0 回退默认值。
	BatchSize int
}

const (
	envReconcileEnabled  = "GROUP_CHANNEL_RECONCILE_ENABLED"
	envReconcileInterval = "GROUP_CHANNEL_RECONCILE_INTERVAL"
	envReconcileGrace    = "GROUP_CHANNEL_RECONCILE_GRACE"
	envReconcileBatch    = "GROUP_CHANNEL_RECONCILE_BATCH"

	defaultReconcileInterval = 5 * time.Minute
	defaultReconcileGrace    = 5 * time.Minute
	defaultReconcileBatch    = 200
	// maxReconcileBatch 防止运维误填巨数把单轮对账变成长时间 IM 风暴。
	maxReconcileBatch = 2000
)

// LoadChannelReconcileConfig 从环境变量装载配置。错误 / 越界值一律回退默认值。
func LoadChannelReconcileConfig() ChannelReconcileConfig {
	return ChannelReconcileConfig{
		Enabled:   parseBoolDefault(os.Getenv(envReconcileEnabled), true),
		Interval:  parsePositiveDuration(os.Getenv(envReconcileInterval), defaultReconcileInterval),
		Grace:     parseNonNegativeDuration(os.Getenv(envReconcileGrace), defaultReconcileGrace),
		BatchSize: parseBoundedPositiveInt(os.Getenv(envReconcileBatch), defaultReconcileBatch, maxReconcileBatch),
	}
}

// parseBoolDefault: 空值回退 def；显式 "false"/"0" → false，"true"/"1" → true，其它回 def。
func parseBoolDefault(raw string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return def
	case "true", "1":
		return true
	case "false", "0":
		return false
	default:
		return def
	}
}

func parsePositiveDuration(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func parseNonNegativeDuration(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return def
	}
	return d
}

func parseBoundedPositiveInt(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
