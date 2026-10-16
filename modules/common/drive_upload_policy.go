package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Drive 上传策略 —— 策略结构与版本（task drive-upload-policy / 设计 §7 M2）。
//
// 本文件把「一个 DB 键 + 一组静态 env 分片参数」收敛成一份对外契约
// （DriveUploadPolicy），供两条下发通道序列化：
//
//	Web  : modules/common/api.go 的 drive_upload_limits（5 字段，无 policy_version）
//	drive: modules/drivepolicy 的 GET /v1/internal/drive/upload-policy（6 字段）
//
// 本仓是这些默认值的**单源**，必须与 ②/③/④ 对齐（共享契约 §3.6）：
// 阈值 128 MiB、片 64 MiB、并发 3、会话 3。
//
// 这些是部署容量 / 协议参数，不是产品语义边界：**不写进 DB、不进管理台**
// （PRD §6.4 V2）。分片参数 env 解析非法 / ≤0 一律回落默认。
const (
	envDriveUploadMultipartThresholdKB  = "OCTO_DRIVE_UPLOAD_MULTIPART_THRESHOLD_KB"
	envDriveUploadPartSizeKB            = "OCTO_DRIVE_UPLOAD_PART_SIZE_KB"
	envDriveUploadMaxConcurrentParts    = "OCTO_DRIVE_UPLOAD_MAX_CONCURRENT_PARTS"
	envDriveUploadMaxConcurrentSessions = "OCTO_DRIVE_UPLOAD_MAX_CONCURRENT_SESSIONS"

	defaultDriveUploadMultipartThresholdKB  = 128 * 1024 // 131072 KiB (128 MiB)
	defaultDriveUploadPartSizeKB            = 64 * 1024  // 65536 KiB (64 MiB)
	defaultDriveUploadMaxConcurrentParts    = 3
	defaultDriveUploadMaxConcurrentSessions = 3
)

// DriveUploadPolicy 是「生效策略」的内容快照，字段名 / 单位与共享契约
// §3.3（服务间接口）逐字一致。policy_version 是下面 6 个输入的**内容哈希**
// （见 driveUploadPolicyVersion），是「生效策略指纹」而不是递增序号：值不变则
// 版本不变，env 分片参数变化（需重启）也会体现。
type DriveUploadPolicy struct {
	MaxSizeKB             int    `json:"max_size_kb"`
	MultipartThresholdKB  int    `json:"multipart_threshold_kb"`
	PartSizeKB            int    `json:"part_size_kb"`
	MaxConcurrentParts    int    `json:"max_concurrent_parts"`
	MaxConcurrentSessions int    `json:"max_concurrent_sessions"`
	PolicyVersion         string `json:"policy_version"`
}

// positiveIntFromEnv 解析一个「必须为正整数」的 env 分片参数：缺省 / 非数字 /
// ≤0 一律回落默认值。照 FileMaxSizeKBHardCap 的防御式解析形态。
func positiveIntFromEnv(getenv func(string) string, key string, fallback int) int {
	if getenv == nil {
		return fallback
	}
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// driveUploadPolicyFrom 是构造策略的**纯函数**（只依赖入参与注入的 getenv，不
// 碰 DB），便于单测直接注入分片参数 env。
//
// maxSizeKB 由调用方在同一份快照上算好；hardCap 与分片参数在这里从 getenv 派生
// —— 它们都是静态 env，不是 DB 键，所以不存在跨快照撕裂。生产路径传 os.Getenv，
// 与 DriveUploadMaxSizeKB() 用的是同一套解析。
func driveUploadPolicyFrom(maxSizeKB int, getenv func(string) string) DriveUploadPolicy {
	lim := driveUploadLimitsFrom(maxSizeKB, getenv)
	hardCap := driveUploadMaxSizeKBHardCapFrom(getenv)
	return DriveUploadPolicy{
		MaxSizeKB:             lim.MaxSizeKB,
		MultipartThresholdKB:  lim.MultipartThresholdKB,
		PartSizeKB:            lim.PartSizeKB,
		MaxConcurrentParts:    lim.MaxConcurrentParts,
		MaxConcurrentSessions: lim.MaxConcurrentSessions,
		PolicyVersion: driveUploadPolicyVersion(
			lim.MaxSizeKB, lim.MultipartThresholdKB, lim.PartSizeKB,
			lim.MaxConcurrentParts, lim.MaxConcurrentSessions, hardCap,
		),
	}
}

// DriveUploadLimits 是生效策略的 **Web 下发视图**：appconfig 下发的 5 个字段，
// 不含 policy_version 指纹。
//
// 单独一个类型（而不是复用 DriveUploadPolicy 再丢掉版本）是为了让「Web 只需要
// 5 个值」在类型上成立：构造它不触发内容哈希，也不会在**未鉴权**的 appconfig
// 热路径上做无谓的 sha256 + 字符串格式化（reviewer yujiawei，PR #930 P2）。指纹
// 只对内部服务间通道有价值（drive 侧按版本日志 / 观测）。
type DriveUploadLimits struct {
	MaxSizeKB             int `json:"max_size_kb"`
	MultipartThresholdKB  int `json:"multipart_threshold_kb"`
	PartSizeKB            int `json:"part_size_kb"`
	MaxConcurrentParts    int `json:"max_concurrent_parts"`
	MaxConcurrentSessions int `json:"max_concurrent_sessions"`
}

// driveUploadLimitsFrom 是构造 Web 视图的纯函数：分片参数从注入的 getenv 派生，
// **不计算指纹**。与 driveUploadPolicyFrom 共享同一套 env 解析与默认值，因此两条
// 通道的 5 个字段逐字节一致。
func driveUploadLimitsFrom(maxSizeKB int, getenv func(string) string) DriveUploadLimits {
	return DriveUploadLimits{
		MaxSizeKB:             maxSizeKB,
		MultipartThresholdKB:  positiveIntFromEnv(getenv, envDriveUploadMultipartThresholdKB, defaultDriveUploadMultipartThresholdKB),
		PartSizeKB:            positiveIntFromEnv(getenv, envDriveUploadPartSizeKB, defaultDriveUploadPartSizeKB),
		MaxConcurrentParts:    positiveIntFromEnv(getenv, envDriveUploadMaxConcurrentParts, defaultDriveUploadMaxConcurrentParts),
		MaxConcurrentSessions: positiveIntFromEnv(getenv, envDriveUploadMaxConcurrentSessions, defaultDriveUploadMaxConcurrentSessions),
	}
}

// driveUploadPolicyVersion 对生效策略的 6 个输入做内容哈希，取 sha256 前 12 个
// hex 字符。顺序固定、无分隔歧义（用 '|' 分隔），值不变则版本不变。
func driveUploadPolicyVersion(maxSizeKB, threshold, partSize, parts, sessions, hardCap int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%d|%d|%d|%d",
		maxSizeKB, threshold, partSize, parts, sessions, hardCap)))
	return hex.EncodeToString(sum[:])[:12]
}

// DriveUploadPolicy 构造当前生效策略。
//
// **单次快照读**：只依赖一个 DB 键 drive.upload_max_size_kb，全部字段出自
// 同一次 s.snapshot.Load() —— 不存在跨键二次 Load 拼出的撕裂组合（对比
// FileStickerSizeOrdering 的两次 snapshot.Load()）。分片参数与 hardCap 是静态
// env，不是 DB 键。
//
// 快照从未成功加载时，返回由代码默认值拼出的策略。Web 下发通道的契约是「值
// 类型恒下发」（06 冻结决策 1），客户端自行按需回落，所以这里保持恒有值。需要
// 区分「快照不可用」的下游（内部服务间通道）改用 DriveUploadPolicyAvail。
func (s *SystemSettings) DriveUploadPolicy() DriveUploadPolicy {
	policy, _ := s.driveUploadPolicyAvail()
	return policy
}

// DriveUploadPolicyAvail 是 DriveUploadPolicy 的可用性感知版本：第二个返回值
// ok 为 false 表示进程内 settings 快照**从未成功加载过**（DB 从未读通），此刻
// 返回的策略是代码默认值而非管理员配置。
//
// 这个区分对内部下发通道是 load-bearing 的：把「不可用配置」当成「合法策略」
// 以 200 + 正常 fingerprint 发给 octo-drive，会让 drive 侧据此抬升上限，破坏
// fail-closed（设计 C2：本项目内部接口的错误契约含 503）。Web 通道不需要它 ——
// 那里恒下发值类型是刻意的。
//
// 一次快照读：ok 与策略 6 字段出自同一次 s.snapshot.Load()。
func (s *SystemSettings) DriveUploadPolicyAvail() (DriveUploadPolicy, bool) {
	return s.driveUploadPolicyAvail()
}

func (s *SystemSettings) driveUploadPolicyAvail() (DriveUploadPolicy, bool) {
	getenv := os.Getenv
	hardCap := driveUploadMaxSizeKBHardCapFrom(getenv)
	maxSizeKB, loaded := s.driveUploadMaxSizeKBFromSnapshot(hardCap)
	return driveUploadPolicyFrom(maxSizeKB, getenv), loaded
}

// DriveUploadLimits 返回 Web 下发通道需要的 5 个字段（单次快照读），**不计算
// policy_version** —— appconfig 不按版本缓存（06 4E / 01 §9-Q6），在未鉴权热路径
// 上做内容哈希没有客户端价值（reviewer yujiawei，PR #930 P2）。
//
// 值类型恒下发（06 冻结决策 1）：快照从未加载时同样回落到默认值 + 天花板钳位；
// 「不可用」只在内部服务间通道上有意义，由 DriveUploadPolicyAvail 承担。
func (s *SystemSettings) DriveUploadLimits() DriveUploadLimits {
	getenv := os.Getenv
	hardCap := driveUploadMaxSizeKBHardCapFrom(getenv)
	maxSizeKB, _ := s.driveUploadMaxSizeKBFromSnapshot(hardCap)
	return driveUploadLimitsFrom(maxSizeKB, getenv)
}
