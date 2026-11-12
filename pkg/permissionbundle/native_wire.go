package permissionbundle

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gocraft/dbr/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// NativeFactSource is the native/SQL adapter boundary, not a policy evaluator.
// Implementations must read actual original Header/current ACL/history/permit
// authorities at their declared cuts and may not derive them from a mirror or
// transport payload. The server supplies its current locking SQL transaction.
type NativeFactSource interface {
	Read(context.Context, *dbr.Tx, Key, []byte) ([]byte, error)
	// ValidateCuts recognizes configured authority/incarnation/position domains
	// and verifies their native acknowledgement-before-read contract. Structural
	// JSON validation or timestamps cannot establish those semantics.
	ValidateCuts(context.Context, Key, []byte) error
}

//go:embed contracts/permission-native-facts-v1.schema.json
var nativeWireSchema []byte
var nativeSchemas struct {
	sync.Once
	request, response map[string]*jsonschema.Schema
	err               error
}

func loadNativeSchemas() {
	nativeSchemas.Do(func() {
		var doc any
		nativeSchemas.err = json.Unmarshal(nativeWireSchema, &doc)
		if nativeSchemas.err != nil {
			return
		}
		c := jsonschema.NewCompiler()
		// Use the artifact's exact ID rather than assuming its spelling.
		id := doc.(map[string]any)["$id"].(string)
		if nativeSchemas.err = c.AddResource(id, doc); nativeSchemas.err != nil {
			return
		}
		nativeSchemas.request = map[string]*jsonschema.Schema{}
		nativeSchemas.response = map[string]*jsonschema.Schema{}
		for op, name := range map[string]string{"resolve": "Resolve", "history_headers": "HistoryHeaders", "history_disclosure": "HistoryDisclosure"} {
			var s *jsonschema.Schema
			s, nativeSchemas.err = c.Compile(id + "#/$defs/" + name + "Request")
			if nativeSchemas.err != nil {
				return
			}
			nativeSchemas.request[op] = s
			s, nativeSchemas.err = c.Compile(id + "#/$defs/" + name + "Response")
			if nativeSchemas.err != nil {
				return
			}
			nativeSchemas.response[op] = s
		}
	})
}
func nativeLexical(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			switch k {
			case "schema_version", "profile_version", "header_version":
				if v != json.Number("1") {
					return false
				}
			case "message_id":
				s, ok := v.(string)
				if !ok {
					return false
				}
				n, e := U64(s)
				if e != nil || n == 0 || n > 9223372036854775807 {
					return false
				}
			case "author_uid", "recipient_bot_uid", "space_id":
				s, ok := v.(string)
				if !ok || !ValidNativeID(s) {
					return false
				}
			case "parent_group":
				if v != nil {
					if s, ok := v.(string); !ok || !ValidNativeID(s) {
						return false
					}
				}
			case "sequence", "context_epoch":
				s, ok := v.(string)
				if !ok {
					return false
				}
				if _, e := U64(s); e != nil {
					return false
				}
			case "page_size":
				n, ok := v.(json.Number)
				if !ok {
					return false
				}
				if _, e := U64(string(n)); e != nil {
					return false
				}
			}
			if !nativeLexical(v) {
				return false
			}
		}
	case []any:
		for _, v := range x {
			if !nativeLexical(v) {
				return false
			}
		}
	}
	return true
}
func nativeRequest(r io.Reader) (map[string]any, []byte, error) {
	v, e := decodeStrict(r, 32768)
	if e != nil {
		return nil, nil, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, ErrInvalid
	}
	op, _ := m["operation"].(string)
	loadNativeSchemas()
	if nativeSchemas.err != nil {
		return nil, nil, ErrUnavailable
	}
	s := nativeSchemas.request[op]
	if s == nil || s.Validate(v) != nil || !nativeLexical(v) {
		return nil, nil, ErrInvalid
	}
	return m, canonical(m), nil
}
func nativeResponse(key Key, request map[string]any, body []byte) ([]byte, error) {
	v, e := decodeStrict(bytes.NewReader(body), MaxBundleBytes)
	if e != nil {
		return nil, ErrUnavailable
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrUnavailable
	}
	s := nativeSchemas.response[request["operation"].(string)]
	if s == nil || s.Validate(v) != nil || !nativeLexical(v) {
		return nil, ErrUnavailable
	}
	k, e := asObject(key)
	if e != nil || !bytes.Equal(canonical(k), canonical(m["key"])) || m["request_id"] != request["request_id"] {
		return nil, ErrUnavailable
	}
	observed, e := utcTime(m["observed_at"])
	if e != nil {
		return nil, e
	}
	ids := map[string]string{}
	var previous []byte
	for _, v := range m["authorities"].([]any) {
		a := v.(map[string]any)
		encoded := canonical(a)
		if previous != nil && bytes.Compare(encoded, previous) <= 0 {
			return nil, ErrUnavailable
		}
		nonce := a["read_nonce"].(string)
		if ids[nonce] != "" {
			return nil, ErrUnavailable
		}
		ids[nonce] = a["kind"].(string)
		previous = encoded
		start, e := utcTime(a["started_at"])
		if e != nil {
			return nil, e
		}
		end, e := utcTime(a["completed_at"])
		if e != nil || end.Before(start) || observed.Before(end) {
			return nil, ErrUnavailable
		}
	}
	if results, ok := m["results"]; ok {
		refs := request["references"]
		field := "delivery_reference"
		if request["operation"] == "history_disclosure" {
			refs = request["headers"]
			field = "header_reference"
		}
		inputs := refs.([]any)
		rows := results.([]any)
		if len(rows) != len(inputs) {
			return nil, ErrUnavailable
		}
		for i, v := range rows {
			row := v.(map[string]any)
			var expected any
			switch input := inputs[i].(type) {
			case map[string]any:
				expected = input[field]
			case string:
				expected = input
			}
			if row["reference"] != expected || len(canonical(row)) > 8192 {
				return nil, ErrUnavailable
			}
			if row["status"] == "resolved" {
				kinds := map[string]bool{}
				for _, id := range row["authority_refs"].([]any) {
					kind := ids[id.(string)]
					kinds[kind] = true
					if kind == "" {
						return nil, ErrUnavailable
					}
				}
				if h, ok := row["header"].(map[string]any); ok {
					if !nativeHeader(key, h) {
						return nil, ErrUnavailable
					}
					if h["author_kind"] == "human" && (!kinds["sql"] || !kinds["im"]) {
						return nil, ErrUnavailable
					}
					if request["operation"] == "resolve" && h["author_kind"] == "human" {
						kind := h["source"].(map[string]any)["kind"].(string)
						purpose, ok := request["purpose"].(string)
						if !ok || row["profile"] != kind+"_"+purpose+"_v1" {
							return nil, ErrUnavailable
						}
					}
				}
				if request["operation"] == "history_disclosure" && (row["context_id"] != request["context_id"] || row["context_epoch"] != request["context_epoch"]) {
					return nil, ErrUnavailable
				}
				if request["operation"] == "history_disclosure" && (!kinds["sql"] || !kinds["im"]) {
					return nil, ErrUnavailable
				}
			}
		}
	}
	if p, ok := m["page"].(map[string]any); ok {
		if p["source_reference"] != request["source_reference"] {
			return nil, ErrUnavailable
		}
		kinds := map[string]bool{}
		for _, nonce := range p["authority_refs"].([]any) {
			kind := ids[nonce.(string)]
			kinds[kind] = true
			if kind == "" {
				return nil, ErrUnavailable
			}
		}
		n, _ := U64(string(request["page_size"].(json.Number)))
		items := p["items"].([]any)
		if uint64(len(items)) > n {
			return nil, ErrUnavailable
		}
		seen := map[string]bool{}
		for _, v := range items {
			h := v.(map[string]any)
			if len(canonical(h)) > 8192 {
				return nil, ErrUnavailable
			}
			if h["header_version"] != nil {
				if !nativeHeader(key, h) {
					return nil, ErrUnavailable
				}
				if h["author_kind"] == "human" && (!kinds["sql"] || !kinds["im"]) {
					return nil, ErrUnavailable
				}
				ref := h["header_reference"].(string)
				if seen[ref] {
					return nil, ErrUnavailable
				}
				seen[ref] = true
			}
		}
	}
	return canonical(m), nil
}
func nativeHeader(key Key, h map[string]any) bool {
	if h["space_id"] != key.SpaceID || h["recipient_bot_uid"] != key.BotUID {
		return false
	}
	s := h["source"].(map[string]any)
	id := s["id"].(string)
	switch s["kind"] {
	case "dm":
		peer, e := dmPeer(key, id)
		if e != nil || h["author_kind"] == "human" && h["author_uid"] != peer {
			return false
		}
	case "group":
		if !ValidNativeID(id) {
			return false
		}
	case "thread":
		parts := threadID.FindStringSubmatch(id)
		if parts == nil || h["parent_group"] != parts[1] {
			return false
		}
	default:
		return false
	}
	return true
}
func utcTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok || !strings.HasSuffix(s, "Z") {
		return time.Time{}, ErrUnavailable
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return time.Time{}, ErrUnavailable
	}
	return t, nil
}

func (r *Runtime) Facts(ctx context.Context, key Key, body io.Reader) ([]byte, error) {
	if r == nil || r.facts == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t, e := r.store.begin(ctx, key, "")
	if e != nil {
		return nil, e
	}
	defer t.tx.RollbackUnlessCommitted()
	req, input, e := nativeRequest(body)
	if e != nil {
		return nil, e
	}
	response, e := r.facts.Read(ctx, t.tx, key, input)
	if e != nil {
		return nil, ErrUnavailable
	}
	out, e := nativeResponse(key, req, response)
	if e != nil {
		return nil, e
	}
	if r.facts.ValidateCuts(ctx, key, out) != nil {
		return nil, ErrUnavailable
	}
	var envelope struct {
		Authorities []struct {
			Kind        string `json:"kind"`
			Incarnation string `json:"incarnation"`
		} `json:"authorities"`
	}
	if json.Unmarshal(out, &envelope) != nil {
		return nil, ErrUnavailable
	}
	for _, a := range envelope.Authorities {
		if a.Kind == "sql" && a.Incarnation != t.authority.Incarnation() {
			return nil, ErrUnavailable
		}
	}
	if e = complete(ctx, t); e != nil {
		return nil, e
	}
	return out, nil
}

func parseDeliveryBindingRequest(r io.Reader) (DeliveryBindingRequestV1, []byte, error) {
	v, e := decodeStrict(r, 8192)
	if e != nil {
		return DeliveryBindingRequestV1{}, nil, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	allowed := map[string]bool{
		"schema_version":          true,
		"operation":               true,
		"request_id":              true,
		"channel_type":            true,
		"channel_id":              true,
		"message_id":              true,
		"message_seq":             true,
		"from_uid":                true,
		"observed_payload_sha256": true,
		"expected_native_version": true,
	}
	for k := range m {
		if !allowed[k] {
			return DeliveryBindingRequestV1{}, nil, ErrInvalid
		}
	}
	if m["schema_version"] != json.Number("1") || m["operation"] != "bind_delivery" {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	reqID, ok := m["request_id"].(string)
	if !ok || !ValidNativeID(reqID) {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	chTypeNum, ok := m["channel_type"].(json.Number)
	if !ok {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	chTypeU64, err := U64(string(chTypeNum))
	if err != nil || (chTypeU64 != channelTypePerson && chTypeU64 != channelTypeGroup && chTypeU64 != channelTypeThread) {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	chID, ok := m["channel_id"].(string)
	if !ok || chID == "" || len(chID) > 256 {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	msgID, ok := m["message_id"].(string)
	if !ok {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	if n, err := U64(msgID); err != nil || n == 0 || n > 9223372036854775807 {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	msgSeq, ok := m["message_seq"].(string)
	if !ok {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	if n, err := U64(msgSeq); err != nil || n == 0 || n > 4294967295 {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	fromUID, ok := m["from_uid"].(string)
	if !ok || !ValidNativeID(fromUID) {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	payloadSHA, ok := m["observed_payload_sha256"].(string)
	if !ok || !validHex(payloadSHA, 64) {
		return DeliveryBindingRequestV1{}, nil, ErrInvalid
	}
	var expectedVer string
	if rawVer, exists := m["expected_native_version"]; exists && rawVer != nil {
		s, ok := rawVer.(string)
		if !ok || s == "" || len(s) > 128 || strings.ContainsAny(s, "- \t\r\n") {
			return DeliveryBindingRequestV1{}, nil, ErrInvalid
		}
		expectedVer = s
	}
	out := DeliveryBindingRequestV1{
		RequestID:             reqID,
		ChannelType:           uint8(chTypeU64),
		ChannelID:             chID,
		MessageID:             msgID,
		MessageSeq:            msgSeq,
		FromUID:               fromUID,
		ObservedPayloadSHA256: payloadSHA,
		ExpectedNativeVersion: expectedVer,
	}
	return out, canonical(m), nil
}

func deliveryBindingResponse(key Key, req DeliveryBindingRequestV1, body []byte) ([]byte, error) {
	v, e := decodeStrict(bytes.NewReader(body), MaxBundleBytes)
	if e != nil {
		return nil, ErrUnavailable
	}
	m, ok := v.(map[string]any)
	if !ok || !nativeLexical(v) {
		return nil, ErrUnavailable
	}
	if m["schema_version"] != json.Number("1") || m["profile_version"] != json.Number("1") {
		return nil, ErrUnavailable
	}
	k, e := asObject(key)
	if e != nil || !bytes.Equal(canonical(k), canonical(m["key"])) || m["request_id"] != req.RequestID {
		return nil, ErrUnavailable
	}
	observed, e := utcTime(m["observed_at"])
	if e != nil {
		return nil, e
	}
	authList, ok := m["authorities"].([]any)
	if !ok || len(authList) == 0 {
		return nil, ErrUnavailable
	}
	ids := map[string]string{}
	var previous []byte
	for _, item := range authList {
		a, ok := item.(map[string]any)
		if !ok {
			return nil, ErrUnavailable
		}
		encoded := canonical(a)
		if previous != nil && bytes.Compare(encoded, previous) <= 0 {
			return nil, ErrUnavailable
		}
		nonce, _ := a["read_nonce"].(string)
		if !validHex(nonce, 32) || ids[nonce] != "" {
			return nil, ErrUnavailable
		}
		ids[nonce], _ = a["kind"].(string)
		previous = encoded
		start, e := utcTime(a["started_at"])
		if e != nil {
			return nil, e
		}
		end, e := utcTime(a["completed_at"])
		if e != nil || end.Before(start) || observed.Before(end) {
			return nil, ErrUnavailable
		}
	}
	binding, ok := m["binding"].(map[string]any)
	if !ok || len(canonical(binding)) > 8192 {
		return nil, ErrUnavailable
	}
	refs, ok := binding["authority_refs"].([]any)
	if !ok || len(refs) == 0 {
		return nil, ErrUnavailable
	}
	kinds := map[string]bool{}
	for _, r := range refs {
		ns, ok := r.(string)
		if !ok || ids[ns] == "" {
			return nil, ErrUnavailable
		}
		kinds[ids[ns]] = true
	}
	if !kinds["sql"] || !kinds["im"] {
		return nil, ErrUnavailable
	}
	status, _ := binding["status"].(string)
	switch status {
	case "refused":
		allowedBinding := map[string]bool{
			"status":                 true,
			"reason":                 true,
			"current":                true,
			"native_message_version": true,
			"authority_refs":         true,
		}
		for bk := range binding {
			if !allowedBinding[bk] {
				return nil, ErrUnavailable
			}
		}
		if cur, ok := binding["current"].(bool); !ok || cur {
			return nil, ErrUnavailable
		}
		if reason, ok := binding["reason"].(string); !ok || reason == "" {
			return nil, ErrUnavailable
		}
	case "bound":
		allowedBinding := map[string]bool{
			"status":                  true,
			"current":                 true,
			"delivery_reference":      true,
			"header_reference":        true,
			"native_message_version":  true,
			"observed_payload_sha256": true,
			"header":                  true,
			"proven_predicates":       true,
			"unknown_predicates":      true,
			"resolvable_purposes":     true,
			"authority_refs":          true,
		}
		for bk := range binding {
			if !allowedBinding[bk] {
				return nil, ErrUnavailable
			}
		}
		if cur, ok := binding["current"].(bool); !ok || !cur {
			return nil, ErrUnavailable
		}
		if binding["observed_payload_sha256"] != req.ObservedPayloadSHA256 {
			return nil, ErrUnavailable
		}
		h, ok := binding["header"].(map[string]any)
		if !ok || !nativeHeader(key, h) || h["payload_sha256"] != req.ObservedPayloadSHA256 {
			return nil, ErrUnavailable
		}
	default:
		return nil, ErrUnavailable
	}
	return canonical(m), nil
}

func (r *Runtime) BindDeliveredMessage(ctx context.Context, key Key, body io.Reader) ([]byte, error) {
	if r == nil || r.facts == nil || !r.HasDeliveryBinding() {
		return nil, ErrUnavailable
	}
	binder, ok := r.facts.(DeliveredMessageBinder)
	if !ok {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	t, e := r.store.begin(ctx, key, "")
	if e != nil {
		return nil, e
	}
	defer t.tx.RollbackUnlessCommitted()
	req, input, e := parseDeliveryBindingRequest(body)
	if e != nil {
		return nil, e
	}
	response, e := binder.BindDeliveredMessage(ctx, t.tx, key, input)
	if e != nil {
		if errors.Is(e, ErrInvalid) {
			return nil, ErrInvalid
		}
		return nil, ErrUnavailable
	}
	out, e := deliveryBindingResponse(key, req, response)
	if e != nil {
		return nil, e
	}
	if r.facts.ValidateCuts(ctx, key, out) != nil {
		return nil, ErrUnavailable
	}
	var envelope struct {
		Authorities []struct {
			Kind        string `json:"kind"`
			Incarnation string `json:"incarnation"`
		} `json:"authorities"`
	}
	if json.Unmarshal(out, &envelope) != nil {
		return nil, ErrUnavailable
	}
	for _, a := range envelope.Authorities {
		if a.Kind == "sql" && a.Incarnation != t.authority.Incarnation() {
			return nil, ErrUnavailable
		}
	}
	if e = complete(ctx, t); e != nil {
		return nil, e
	}
	return out, nil
}
