package cardactiondispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	HeaderSignature = "X-Octo-Signature"
	HeaderTimestamp = "X-Octo-Timestamp"
	HeaderEventID   = "X-Octo-Event-ID"
)

type HTTPDeliverer struct {
	client *http.Client
	clock  func() time.Time
}

type DeliveryError struct {
	Category  string
	Status    int
	retryable bool
	cause     error
}

func (e *DeliveryError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("cardactiondispatch: callback %s (status %d)", e.Category, e.Status)
	}
	return "cardactiondispatch: callback " + e.Category
}

func (e *DeliveryError) Unwrap() error { return e.cause }

func Retryable(err error) bool {
	var deliveryErr *DeliveryError
	return errors.As(err, &deliveryErr) && deliveryErr.retryable
}

func NewHTTPDeliverer(transport http.RoundTripper, clock func() time.Time) *HTTPDeliverer {
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		// Routes are already exact, static HTTPS URLs. Ignoring proxy environment
		// variables prevents deployment-level proxy settings from reopening SSRF.
		base.Proxy = nil
		transport = base
	}
	if clock == nil {
		clock = time.Now
	}
	return &HTTPDeliverer{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		clock: clock,
	}
}

func (d *HTTPDeliverer) Deliver(ctx context.Context, route *Route, request DecisionRequest) (DecisionResult, error) {
	if ctx == nil || route == nil {
		return DecisionResult{}, &DeliveryError{Category: "invalid_request"}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return DecisionResult{}, &DeliveryError{Category: "encode_failed", cause: err}
	}
	parsed, err := url.Parse(route.URL)
	if err != nil {
		return DecisionResult{}, &DeliveryError{Category: "invalid_route", cause: err}
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	timestamp := strconv.FormatInt(d.clock().Unix(), 10)
	eventID := strconv.FormatInt(request.EventID, 10)

	requestCtx, cancel := context.WithTimeout(ctx, route.Timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, route.URL, bytes.NewReader(body))
	if err != nil {
		return DecisionResult{}, &DeliveryError{Category: "request_failed", cause: err}
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "octo-server/card-action-dispatch-v1")
	httpRequest.Header.Set(HeaderTimestamp, timestamp)
	httpRequest.Header.Set(HeaderEventID, eventID)
	httpRequest.Header.Set(HeaderSignature, Sign(route.secret, http.MethodPost, path, timestamp, eventID, body))

	response, err := d.client.Do(httpRequest)
	if err != nil {
		retry := ctx.Err() == nil
		return DecisionResult{}, &DeliveryError{Category: "transport_failed", retryable: retry, cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		retry := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		category := "rejected"
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			category = "redirect_rejected"
		} else if response.StatusCode >= 500 {
			category = "consumer_5xx"
		}
		return DecisionResult{}, &DeliveryError{Category: category, Status: response.StatusCode, retryable: retry}
	}
	result, err := DecodeDecisionResult(response.Body)
	if err != nil {
		return DecisionResult{}, &DeliveryError{Category: "invalid_response", retryable: true, cause: err}
	}
	return result, nil
}
