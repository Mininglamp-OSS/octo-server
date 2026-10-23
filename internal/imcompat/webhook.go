package imcompat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// WebhookHandler accepts unsigned v3 events only from the same network namespace.
// The executable must bind this handler to a numeric loopback address. The
// downstream Server retains its existing HMAC authentication and business checks.
func (a *Adapter) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LoopbackAddress(r.RemoteAddr) {
			protocolError(w, http.StatusForbidden, "local IM callback required")
			return
		}
		if r.URL.Path != "/webhook" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			protocolError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		if !acquire(a.webhookSlots) {
			protocolError(w, http.StatusServiceUnavailable, "webhook adapter is busy")
			return
		}
		defer func() { <-a.webhookSlots }()
		body, err := readBounded(r.Body, a.opts.MaxBodyBytes)
		if err != nil {
			protocolError(w, http.StatusRequestEntityTooLarge, "invalid or oversized webhook")
			return
		}
		event := r.URL.Query().Get("event")
		body, ids, err := normalizeEvent(event, body)
		if err != nil {
			protocolError(w, http.StatusBadRequest, "invalid webhook event")
			return
		}
		mac := hmac.New(sha256.New, []byte(a.opts.WebhookSecret))
		_, _ = mac.Write(body)
		headers := http.Header{"X-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}
		target := *a.serverWebhook
		query := target.Query()
		query.Set("event", event)
		target.RawQuery = query.Encode()
		status, response, err := a.request(r.Context(), http.MethodPost, &target, body, headers)
		if err != nil || status != http.StatusOK || !validWebhookAck(event, response, ids) {
			protocolError(w, http.StatusBadGateway, "Server webhook was not acknowledged")
			return
		}
		// No local queue or early acknowledgement: v3 may retry only after the
		// existing Server has rejected, timed out, or incompletely acknowledged.
		writeJSON(w, http.StatusOK, map[string]int{"status": http.StatusOK})
	})
}

func normalizeEvent(event string, body []byte) ([]byte, []string, error) {
	switch event {
	case "msg.notify":
		var messages []map[string]json.RawMessage
		if json.Unmarshal(body, &messages) != nil || messages == nil || len(messages) > MaxEventItems {
			return nil, nil, errors.New("invalid notify batch")
		}
		ids := make([]string, 0, len(messages))
		for _, message := range messages {
			if err := normalizePerson(message); err != nil {
				return nil, nil, err
			}
			var id int64
			if json.Unmarshal(message["message_id"], &id) != nil || id <= 0 {
				return nil, nil, errors.New("invalid message ID")
			}
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		out, err := json.Marshal(messages)
		return out, ids, err
	case "msg.offline":
		var message map[string]json.RawMessage
		if json.Unmarshal(body, &message) != nil || message == nil {
			return nil, nil, errors.New("invalid offline event")
		}
		if err := normalizePerson(message); err != nil {
			return nil, nil, err
		}
		var recipients []string
		if raw, ok := message["to_uids"]; ok && (json.Unmarshal(raw, &recipients) != nil || len(recipients) > MaxEventItems) {
			return nil, nil, errors.New("invalid offline recipients")
		}
		out, err := json.Marshal(message)
		return out, nil, err
	case "user.onlinestatus":
		var statuses []string
		if json.Unmarshal(body, &statuses) != nil || statuses == nil || len(statuses) > MaxEventItems {
			return nil, nil, errors.New("invalid online status batch")
		}
		return body, nil, nil
	default:
		return nil, nil, errors.New("unsupported webhook event")
	}
}

func normalizePerson(message map[string]json.RawMessage) error {
	var channelType uint8
	var channelID, fromUID string
	if json.Unmarshal(message["channel_type"], &channelType) != nil || channelType == 0 ||
		json.Unmarshal(message["channel_id"], &channelID) != nil || channelID == "" ||
		json.Unmarshal(message["from_uid"], &fromUID) != nil {
		return errors.New("invalid message channel")
	}
	if channelType != 1 || !strings.Contains(channelID, "@") {
		return nil
	}
	parts := strings.Split(channelID, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || fromUID == "" {
		return errors.New("invalid Person channel")
	}
	var peer string
	switch fromUID {
	case parts[0]:
		peer = parts[1]
	case parts[1]:
		peer = parts[0]
	default:
		return errors.New("sender is not a Person channel participant")
	}
	message["channel_id"], _ = json.Marshal(peer)
	return nil
}

func validWebhookAck(event string, body []byte, expectedIDs []string) bool {
	if event == "msg.notify" {
		var actual []string
		if json.Unmarshal(body, &actual) != nil || actual == nil || len(actual) != len(expectedIDs) {
			return false
		}
		counts := make(map[string]int, len(expectedIDs))
		for _, id := range expectedIDs {
			counts[id]++
		}
		for _, id := range actual {
			if counts[id] <= 0 {
				return false
			}
			counts[id]--
		}
		return true
	}
	var result struct {
		Status int `json:"status"`
	}
	return json.Unmarshal(body, &result) == nil && result.Status == http.StatusOK
}
