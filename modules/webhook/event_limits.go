package webhook

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// 回调事件的结构性上限。它们限制单个事件能触发的工作量，不是流量整形：
// 取值远高于正常批次，超出时整个事件报错，不做截断。
//
// gRPC 通道默认单条消息 4 MiB，已隐式限制了未压缩列表的大小；这些上限同时覆盖
// HTTP 通道，以及 gzip 压缩后展开的收件人列表。
const (
	// maxOfflineRecipients 是单个 msg.offline 事件去重后的最大收件人数。
	// WuKongIM 按频道一次性下发全部离线成员，不分批，因此取值需覆盖大群。
	maxOfflineRecipients = 200_000
	// maxOfflineRecipientEntries 是去重前的最大条目数，给重复项留出一倍余量。
	// 它限制解析和去重的工作量：大量重复的短 uid 不会先被全部展开再被拒绝。
	maxOfflineRecipientEntries = 2 * maxOfflineRecipients
	// maxOfflineRecipientsDecompressedBytes 是 msg.offline 压缩收件人列表解压后的最大字节数，
	// 与 maxOfflineRecipients 对齐：20 万个 uid、每个连同引号和逗号约 80 字节以内。
	maxOfflineRecipientsDecompressedBytes = 16 << 20 // 16 MiB
	// maxOnlineStatusEntries 是单个 user.onlinestatus 事件的最大条目数。
	// WuKongIM 会把积压的状态变更一次性发出，取值高于 4 MiB 消息可容纳的常见条目数。
	maxOnlineStatusEntries = 100_000
)

// decodeCompressedRecipients 解压并解析 gzip 压缩的收件人列表。
// 解压后超过 maxOfflineRecipientsDecompressedBytes 字节，或条目数超过
// maxOfflineRecipientEntries 时返回错误；条目逐个解码，超限时立即停止。
func decodeCompressedRecipients(compressed []byte) ([]string, error) {
	gReader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("解码gzip失败: %w", err)
	}
	defer gReader.Close()
	data, err := io.ReadAll(io.LimitReader(gReader, maxOfflineRecipientsDecompressedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取gzip压缩数据失败: %w", err)
	}
	if len(data) > maxOfflineRecipientsDecompressedBytes {
		return nil, fmt.Errorf("压缩收件人列表解压后超过上限 %d 字节", maxOfflineRecipientsDecompressedBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("解析压缩收件人列表失败: %w", err)
	}
	if tok == nil { // JSON null，与之前的解码行为一致：没有收件人
		return nil, nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, errors.New("解析压缩收件人列表失败: 不是 JSON 数组")
	}
	toUids := make([]string, 0)
	for dec.More() {
		if len(toUids) == maxOfflineRecipientEntries {
			return nil, fmt.Errorf("离线推送收件人条目数超过上限 %d（去重前）", maxOfflineRecipientEntries)
		}
		var uid string
		if err := dec.Decode(&uid); err != nil {
			return nil, fmt.Errorf("解析压缩收件人列表失败: %w", err)
		}
		toUids = append(toUids, uid)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("解析压缩收件人列表失败: %w", err)
	}
	return toUids, nil
}

// normalizeOfflineRecipients 按首次出现顺序去重收件人。
// 去重前超过 maxOfflineRecipientEntries，或去重后超过 maxOfflineRecipients 时返回错误；
// 去重超限时立即返回，去重集合的大小因此不超过上限。错误中带有输入条目数，便于排查。
func normalizeOfflineRecipients(toUids []string) ([]string, error) {
	if len(toUids) > maxOfflineRecipientEntries {
		return nil, fmt.Errorf("离线推送收件人条目数 %d 超过上限 %d（去重前）", len(toUids), maxOfflineRecipientEntries)
	}
	capacity := len(toUids)
	if capacity > maxOfflineRecipients {
		capacity = maxOfflineRecipients
	}
	seen := make(map[string]struct{}, capacity)
	out := make([]string, 0, capacity)
	for _, uid := range toUids {
		if _, ok := seen[uid]; ok {
			continue
		}
		if len(out) == maxOfflineRecipients {
			return nil, fmt.Errorf("离线推送收件人数超过上限 %d（去重后；输入 %d 条）", maxOfflineRecipients, len(toUids))
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out, nil
}
