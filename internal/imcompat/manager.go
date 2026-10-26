package imcompat

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

func (a *Adapter) maxMessageSeq(w http.ResponseWriter, r *http.Request) {
	if a.manager == nil {
		protocolError(w, http.StatusNotImplemented, "maximum sequence requires the read-only Manager account")
		return
	}
	channelID := r.URL.Query().Get("channel_id")
	channelType, err := strconv.ParseUint(r.URL.Query().Get("channel_type"), 10, 8)
	if strings.TrimSpace(channelID) == "" || err != nil || channelType == 0 {
		protocolError(w, http.StatusBadRequest, "invalid channel")
		return
	}
	// This infrequent control operation logs in per request: no persistent token
	// cache, expiry assumption, privilege bypass, or write operation is needed.
	loginBody, _ := json.Marshal(map[string]string{"username": a.opts.ManagerUser, "password": a.opts.ManagerPassword})
	status, body, err := a.request(r.Context(), http.MethodPost, endpoint(a.manager, "/manager/login"), loginBody, nil)
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &login) != nil || login.AccessToken == "" {
		protocolError(w, http.StatusBadGateway, "Manager authentication unavailable")
		return
	}
	target := endpoint(a.manager, "/manager/channel-runtime-meta")
	query := target.Query()
	query.Set("exact", "true")
	query.Set("include_max_message_seq", "true")
	query.Set("channel_id", channelID)
	query.Set("channel_type", strconv.FormatUint(channelType, 10))
	target.RawQuery = query.Encode()
	status, body, err = a.request(r.Context(), http.MethodGet, target, nil, http.Header{"Authorization": {"Bearer " + login.AccessToken}})
	var result struct {
		Items []struct {
			ChannelID     string  `json:"channel_id"`
			ChannelType   uint8   `json:"channel_type"`
			MaxMessageSeq *uint64 `json:"max_message_seq"`
		} `json:"items"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &result) != nil || len(result.Items) != 1 {
		protocolError(w, http.StatusBadGateway, "committed channel sequence unavailable")
		return
	}
	item := result.Items[0]
	if item.ChannelID != channelID || uint64(item.ChannelType) != channelType || item.MaxMessageSeq == nil || *item.MaxMessageSeq > math.MaxUint32 {
		protocolError(w, http.StatusBadGateway, "invalid or unsupported committed channel sequence")
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"message_seq": *item.MaxMessageSeq})
}
