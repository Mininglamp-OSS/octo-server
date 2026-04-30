package notify

// NotifyReq 通知请求
type NotifyReq struct {
	SpaceID  string                 `json:"space_id" binding:"required"`
	Service  string                 `json:"service" binding:"required"`
	Event    string                 `json:"event"`
	Targets  []string               `json:"targets" binding:"required"`
	ActorUID string                 `json:"actor_uid"`
	Payload  map[string]interface{} `json:"payload" binding:"required"`
}

// BatchNotifyReq 批量通知请求
type BatchNotifyReq struct {
	Notifications []NotifyReq `json:"notifications" binding:"required"`
}

// NotifyResp 单条通知响应
type NotifyResp struct {
	Delivered []string          `json:"delivered"`
	Filtered  map[string]string `json:"filtered"`
}

// BatchNotifyResult 批量通知中单条结果
type BatchNotifyResult struct {
	NotifyResp
	Error string `json:"error,omitempty"`
}

// BatchNotifyResp 批量通知响应
type BatchNotifyResp struct {
	Results   []BatchNotifyResult `json:"results"`
	HasErrors bool                `json:"has_errors"`
}
