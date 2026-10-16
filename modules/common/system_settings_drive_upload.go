package common

import (
	"os"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Drive 上传策略 —— 配置真源（task drive-upload-policy）。
//
// 本文件只负责「单个上传上限」这一格的读侧真源：DB 键 drive.upload_max_size_kb
// + 部署 env 天花板 OCTO_DRIVE_UPLOAD_MAX_SIZE_KB_HARD_CAP + 代码默认值，三层
// 回退与 clamp。分片参数与策略结构在 drive_upload_policy.go，appconfig 下发在
// api.go，服务间下发在 modules/drivepolicy。
//
// 这是「配置真源」而不是「上传校验」：octo-server 不代理文件字节、不做上传
// 大小强制（那是 ② octo-drive 的职责），这里只保证下发值正确。
//
// 职责三层（照 file.max_size_kb 的既有形态）：
//
//	开发   代码默认值 DefaultDriveUploadMaxSizeKB（1 GiB），未配置时兜底
//	运维   OCTO_DRIVE_UPLOAD_MAX_SIZE_KB_HARD_CAP 定天花板（改 configmap + 重启）
//	超管   drive.upload_max_size_kb 调实际值（管理台，≤60s 生效，超不过天花板）
//
// 天花板若是 env 而不是管理台：超管就能先抬天花板再抬实际值，这道闸等于不
// 存在。env 要重启 Pod，这个成本本身就是防线。
const (
	// DefaultDriveUploadMaxSizeKB 是单文件上限的代码默认值（1 GiB）。
	// drive.upload_max_size_kb 未配置时生效。这个键没有历史 env 兼容层 —— 它
	// 是本任务全新引入的键，回退链只有 DB → code default 两层。
	DefaultDriveUploadMaxSizeKB = 1024 * 1024

	// envDriveUploadMaxSizeKBHardCap 让部署自己决定 drive 单文件上限的**天花板**。
	envDriveUploadMaxSizeKBHardCap = "OCTO_DRIVE_UPLOAD_MAX_SIZE_KB_HARD_CAP"

	// defaultDriveUploadMaxSizeKBHardCap 是未配置 env 时的天花板（10 GiB）。
	defaultDriveUploadMaxSizeKBHardCap = 10 * 1024 * 1024

	// driveUploadMaxSizeKBAbsoluteMax 只防溢出，不表达策略：maxKB 会以
	// int64(maxKB)*1024 转成字节，一个荒谬的 env 值会让它绕回负数，而负的上限
	// 意味着**所有上传都被拒**。100 GiB 远超任何真实场景，又离溢出边界足够远；
	// 它也必须高于默认天花板（10 GiB），否则默认值会被自己的防溢出上界判为非法。
	driveUploadMaxSizeKBAbsoluteMax = 100 * 1024 * 1024
)

// driveUploadMaxSizeKBHardCapFrom 从注入的 getenv 解析天花板，便于单测。
//
// 非法值（非数字 / ≤0 / 超过防溢出上界）回落代码默认值，而不是被原样服务。
// getenv 为 nil 同样回落默认值（fail-safe，不 panic）。
func driveUploadMaxSizeKBHardCapFrom(getenv func(string) string) int {
	if getenv == nil {
		return defaultDriveUploadMaxSizeKBHardCap
	}
	raw := strings.TrimSpace(getenv(envDriveUploadMaxSizeKBHardCap))
	if raw == "" {
		return defaultDriveUploadMaxSizeKBHardCap
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 || v > driveUploadMaxSizeKBAbsoluteMax {
		return defaultDriveUploadMaxSizeKBHardCap
	}
	return v
}

// DriveUploadMaxSizeKBHardCap 返回 drive 单文件上限的天花板（KB）。
//
// 这是**部署容量决策**，不是产品语义边界，所以跟着部署配置走而不是编译进二
// 进制：能传多大取决于对象存储后端、带宽与临时磁盘，每个部署不一样。非法
// env（非数字 / ≤0 / 超防溢出上界）回落默认 10 GiB，不 panic、不暴露非法值。
func DriveUploadMaxSizeKBHardCap() int {
	return driveUploadMaxSizeKBHardCapFrom(os.Getenv)
}

// driveUploadMaxSizeKBFromSnapshot 在**一次快照读**内解析 drive.upload_max_size_kb
// 并钳到 hardCap。返回值第二项 loaded 报告该快照是否**成功加载过**：
//
//	loaded=false  s.snapshot 为 nil，即设置表从未成功读过（不是「管理员没配」）。
//	              Web 下发通道仍需一个值（值类型恒下发），所以照旧回落
//	              min(默认, hardCap)；但内部服务间通道必须据此 fail-closed，
//	              见 DriveUploadPolicyAvail。
//	loaded=true   快照已发布（可能为空 map）：未配置 → min(默认, hardCap)；
//	              已配置 → clampIntUpper。
//
// 未配置那一格刻意不走钳位器：天花板可以低于代码默认值，把默认值喂进去会报
// configured，而 system_setting 表里一行都没有 —— 那是在诬告一次没发生过的
// 变更（与 FileMaxSizeKB 同理由）。回落值本身仍要过天花板。
func (s *SystemSettings) driveUploadMaxSizeKBFromSnapshot(hardCap int) (int, bool) {
	snapPtr := s.snapshot.Load()
	if snapPtr == nil {
		return min(DefaultDriveUploadMaxSizeKB, hardCap), false
	}
	raw, ok := (*snapPtr)[schemaKey("drive", "upload_max_size_kb")]
	if !ok || raw == "" {
		return min(DefaultDriveUploadMaxSizeKB, hardCap), true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		// 键存在但解析不了：确实配置过、只是非法。交给钳位器的 ≤0 分支回落，
		// 与 getIntOK 的 (0, true) 语义逐字节一致。
		v = 0
	}
	return s.clampIntUpper("drive.upload_max_size_kb", v, DefaultDriveUploadMaxSizeKB, hardCap), true
}

// DriveUploadMaxSizeKB 返回 drive 单文件上传上限（KB）。回退链只有
// DB → code default 两层；越界处理与 sticker / file 那组键共用同一个钳位器。
//
// 快照从未加载时返回 min(默认, hardCap)：这是 Web 下发通道要的「恒有一个值」，
// 不是「配置权威」的断言 —— 需要区分两者的调用方用 DriveUploadPolicyAvail。
func (s *SystemSettings) DriveUploadMaxSizeKB() int {
	v, _ := s.driveUploadMaxSizeKBFromSnapshot(DriveUploadMaxSizeKBHardCap())
	return v
}
