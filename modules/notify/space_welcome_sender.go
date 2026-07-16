package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// space_welcome_sender.go — a notify-local, context-aware HTTP sender for the
// WuKongIM /message/send endpoint.
//
// Why not octo-lib's SendMessageWithResult: that helper neither accepts a
// context nor sets an HTTP client timeout (the underlying sendgrid rest.API
// uses no timeout), so wrapping it in context.WithTimeout cannot actually
// interrupt a hung request — the socket lingers. This sender uses
// http.NewRequestWithContext + a shared http.Client{Timeout: 15s}, so a timeout
// genuinely closes the socket and unblocks the worker, and 15s < 30s lease
// guarantees a hung request cannot outlive its claim.
//
// octo-lib is NOT modified: we only read exported config accessors and mirror
// the stable response body parse from config/msg.go.

// swSendResult is the classified outcome of one /message/send call.
type swSendResult struct {
	messageID   int64
	clientMsgNo string
	messageSeq  uint32
}

// swSendError is a transport/protocol error classified into an error_class the
// worker maps to a ledger transition (im_timeout / im_bad_response).
type swSendError struct {
	class string
	err   error
}

func (e *swSendError) Error() string {
	if e.err == nil {
		return e.class
	}
	return e.class + ": " + e.err.Error()
}

func (e *swSendError) Unwrap() error { return e.err }

// spaceWelcomeSender posts personal DM messages to WuKongIM with a hard timeout.
type spaceWelcomeSender struct {
	ctx    *config.Context
	client *http.Client
}

func newSpaceWelcomeSender(ctx *config.Context) *spaceWelcomeSender {
	return &spaceWelcomeSender{
		ctx:    ctx,
		client: &http.Client{Timeout: swHTTPTimeout},
	}
}

// send posts req to <APIURL>/message/send. On 200 OK it parses
// data.message_id / data.client_msg_no / data.message_seq (mirroring
// config/msg.go). Any non-200, transport error, or unparseable body is returned
// as a *swSendError classified for the worker's unknown-vs-retry decision.
func (s *spaceWelcomeSender) send(ctx context.Context, req *config.MsgSendReq) (*swSendResult, error) {
	cfg := s.ctx.GetConfig()
	apiURL := cfg.WuKongIM.APIURL
	if apiURL == "" {
		return nil, &swSendError{class: swErrIMBadResponse, err: errors.New("WuKongIM APIURL not configured")}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, &swSendError{class: swErrIMBadResponse, err: fmt.Errorf("marshal send req: %w", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/message/send", bytes.NewReader(body))
	if err != nil {
		return nil, &swSendError{class: swErrIMBadResponse, err: fmt.Errorf("build send request: %w", err)}
	}
	// Content-Type must be set explicitly in addition to the manager token —
	// the token header map only carries {"token": ...}.
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.WuKongIMManagerTokenHeaderMap() {
		httpReq.Header.Set(k, v)
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		// Timeout / connection reset / DNS — transport-ambiguous.
		return nil, &swSendError{class: swErrIMTimeout, err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, &swSendError{class: swErrIMTimeout, err: fmt.Errorf("read send response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &swSendError{class: swErrIMBadResponse, err: fmt.Errorf("IM /message/send status %d", resp.StatusCode)}
	}

	// Parse the stable response shape { "data": { message_id, client_msg_no,
	// message_seq } } — same fields octo-lib's config/msg.go reads.
	var parsed struct {
		Data struct {
			MessageID   int64  `json:"message_id"`
			ClientMsgNo string `json:"client_msg_no"`
			MessageSeq  uint32 `json:"message_seq"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, &swSendError{class: swErrIMBadResponse, err: fmt.Errorf("decode send response: %w", err)}
	}
	if parsed.Data.MessageID == 0 && parsed.Data.ClientMsgNo == "" {
		// Empty/degenerate body on a 200 — treat as transport-ambiguous rather
		// than a confirmed success (we cannot prove the message persisted).
		return nil, &swSendError{class: swErrIMBadResponse, err: errors.New("empty message_id/client_msg_no in 200 response")}
	}
	return &swSendResult{
		messageID:   parsed.Data.MessageID,
		clientMsgNo: parsed.Data.ClientMsgNo,
		messageSeq:  parsed.Data.MessageSeq,
	}, nil
}
