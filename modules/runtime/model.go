package runtime

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
)

type agentRuntimeModel struct {
	SpaceID     string
	DaemonID    string
	Name        string
	Provider    string
	RuntimeMode string
	Status      string
	Version     string
	DeviceName  string
	DeviceInfo  string
	Metadata    string
	OwnerUID    string
	LastSeenAt  db.Time
	db.BaseModel
}

type registerReq struct {
	DaemonID   string       `json:"daemon_id"`
	DeviceName string       `json:"device_name"`
	DeviceInfo string       `json:"device_info"`
	CLIVersion string       `json:"cli_version"`
	Runtimes   []runtimeReq `json:"runtimes"`
}

type runtimeReq struct {
	Name    string       `json:"name"`
	Type    string       `json:"type"`
	Version string       `json:"version"`
	Status  string       `json:"status"`
	Agents  []agentInfo  `json:"agents,omitempty"`
	Plugins []pluginInfo `json:"plugins,omitempty"`
}

type pluginInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type agentInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Bindings int      `json:"bindings"`
	Default  bool     `json:"is_default"`
	Routes   []string `json:"routes,omitempty"`
}

type heartbeatReq struct {
	RuntimeID int64 `json:"runtime_id"`
}

type deregisterReq struct {
	RuntimeIDs []int64 `json:"runtime_ids"`
}

type runtimeResp struct {
	ID          int64  `json:"id"`
	SpaceID     string `json:"space_id"`
	DaemonID    string `json:"daemon_id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	RuntimeMode string `json:"runtime_mode"`
	Status      string `json:"status"`
	Version     string `json:"version"`
	DeviceName  string `json:"device_name"`
	DeviceInfo  string `json:"device_info"`
	Metadata    string `json:"metadata"`
	OwnerUID    string `json:"owner_uid"`
	LastSeenAt  string `json:"last_seen_at"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type registeredRuntimeResp struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
}

type pingInitReq struct {
	DaemonID string `json:"daemon_id"`
	SpaceID  string `json:"space_id"`
}

type upgradeInitReq struct {
	DaemonID  string `json:"daemon_id"`
	SpaceID   string `json:"space_id"`
	Component string `json:"component"`            // 默认 "octo-daemon"；插件填 "octo"
	RuntimeID int64  `json:"runtime_id,omitempty"` // 插件分支必填：对应 openclaw runtime 的 id
}

type upgradeReportReq struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

type activeUpgradeItem struct {
	TaskID      string `json:"task_id"`
	DaemonID    string `json:"daemon_id"`
	Component   string `json:"component"`
	RuntimeID   int64  `json:"runtime_id,omitempty"` // 插件有值，daemon 升级无值
	Status      string `json:"status"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	ErrorMsg    string `json:"error_msg"`
}

type upgradeTask struct {
	ID          string `db:"id"`
	SpaceID     string `db:"space_id"`
	DaemonID    string `db:"daemon_id"`
	OwnerUID    string `db:"owner_uid"`
	Component   string `db:"component"`
	FromVersion string `db:"from_version"`
	ToVersion   string `db:"to_version"`
	DownloadURL string `db:"download_url"`
	Checksum    string `db:"checksum"`
	Metadata    string `db:"metadata"`
	Status      string `db:"status"`
	ErrorMsg    string `db:"error_msg"`
}

type releaseMetaJSON struct {
	Tag       string              `json:"tag"`
	Assets    []releaseAssetJSON  `json:"assets"`
	Checksums map[string]string   `json:"checksums"`
}

type releaseAssetJSON struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Kind string `json:"kind"`
}

type pingResultReq struct {
	DaemonTS int64 `json:"daemon_ts"`
}

type pingEntry struct {
	ID       string `db:"id"`
	SpaceID  string `db:"space_id"`
	DaemonID string `db:"daemon_id"`
	ServerTS int64  `db:"server_ts"`
	DaemonTS int64  `db:"daemon_ts"`
	RTT      int64  `db:"rtt_ms"`
	Status   string `db:"status"`
}
