package imcompat

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// RawMessage prevents JSON message IDs above 2^53 from passing through float64.
func legacySendBody(body []byte) (int, []byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return 0, nil, errors.New("invalid send response")
	}
	if data, ok := fields["data"]; ok {
		var legacy struct {
			MessageID   int64  `json:"message_id"`
			ClientMsgNo string `json:"client_msg_no"`
		}
		if json.Unmarshal(data, &legacy) != nil || legacy.MessageID == 0 || legacy.ClientMsgNo == "" {
			return 0, nil, errors.New("invalid legacy send response")
		}
		return http.StatusOK, body, nil
	}
	var result struct {
		Reason      *uint8  `json:"reason"`
		MessageID   *int64  `json:"message_id"`
		MessageSeq  *uint32 `json:"message_seq"`
		ClientMsgNo string  `json:"client_msg_no"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Reason == nil {
		return 0, nil, errors.New("invalid v3 send response")
	}
	if *result.Reason != 1 {
		out, err := json.Marshal(map[string]any{
			"status": http.StatusBadRequest, "reason": *result.Reason,
			"msg": fmt.Sprintf("IM send rejected (reason=%d)", *result.Reason),
		})
		return http.StatusBadRequest, out, err
	}
	if result.MessageID == nil || result.MessageSeq == nil || result.ClientMsgNo == "" || *result.MessageID <= 0 {
		return 0, nil, errors.New("incomplete v3 send acknowledgement")
	}
	out, err := json.Marshal(map[string]any{"status": http.StatusOK, "data": fields})
	return http.StatusOK, out, err
}

func (a *Adapter) modifySendResponse(resp *http.Response) error {
	if resp.Request.URL.Path != "/message/send" || resp.Request.Method != http.MethodPost || resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := readBounded(resp.Body, a.opts.MaxBodyBytes)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	status, result, err := legacySendBody(body)
	if err != nil {
		return err
	}
	resp.StatusCode = status
	resp.Status = strconv.Itoa(status) + " " + http.StatusText(status)
	resp.Body = io.NopCloser(bytes.NewReader(result))
	resp.ContentLength = int64(len(result))
	resp.Header.Set("Content-Length", strconv.Itoa(len(result)))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("ETag")
	return nil
}

func (a *Adapter) sendBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Header      json.RawMessage `json:"header"`
		FromUID     string          `json:"from_uid"`
		Subscribers []string        `json:"subscribers"`
		Payload     []byte          `json:"payload"`
	}
	body, readErr := readBounded(r.Body, a.opts.MaxBodyBytes)
	if err := json.Unmarshal(body, &req); readErr != nil || err != nil || strings.TrimSpace(req.FromUID) == "" ||
		len(req.Payload) == 0 || len(req.Subscribers) == 0 || len(req.Subscribers) > MaxEventItems {
		protocolError(w, http.StatusBadRequest, "invalid batch send request (maximum 1000 recipients)")
		return
	}
	for _, uid := range req.Subscribers {
		if strings.TrimSpace(uid) == "" {
			protocolError(w, http.StatusBadRequest, "batch recipient must not be empty")
			return
		}
	}
	failUIDs, reasons := make([]string, 0), make([]string, 0)
	for _, uid := range req.Subscribers {
		body, err := json.Marshal(map[string]any{
			"header": req.Header, "from_uid": req.FromUID, "channel_id": uid,
			"channel_type": 1, "payload": req.Payload, "client_msg_no": "compat-" + rand.Text(),
		})
		var failure string
		if err != nil || r.Context().Err() != nil {
			failure = "batch deadline reached; recipient not attempted"
		} else {
			status, response, requestErr := a.request(r.Context(), http.MethodPost, endpoint(a.core, "/message/send"), body, nil)
			if requestErr != nil {
				failure = requestErr.Error()
			} else if status != http.StatusOK {
				failure = fmt.Sprintf("IM send returned HTTP %d", status)
			} else {
				status, response, err = legacySendBody(response)
				if err != nil {
					failure = "invalid IM acknowledgement; delivery may be unknown"
				} else if status != http.StatusOK {
					var rejected struct {
						Message string `json:"msg"`
					}
					_ = json.Unmarshal(response, &rejected)
					failure = rejected.Message
				}
			}
		}
		if failure != "" {
			failUIDs, reasons = append(failUIDs, uid), append(reasons, failure)
		}
	}
	status := http.StatusOK
	result := map[string]any{"fail_uids": failUIDs, "reason": reasons}
	if len(failUIDs) != 0 {
		// octo-lib ignores fail_uids on HTTP 200. Preserve the partial outcomes,
		// but make the call fail; never retry the whole non-atomic fanout here.
		status = http.StatusBadRequest
		result["status"], result["msg"] = status, "IM batch send partially failed; do not retry successful recipients"
	}
	writeJSON(w, status, result)
}
