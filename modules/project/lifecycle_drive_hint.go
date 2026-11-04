package project

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/projectprovision"
	"go.uber.org/zap"
)

type driveAccessChangedClient struct {
	endpoint string
	token    string
	client   *http.Client
}

func newDriveAccessChangedClient(endpoint, token string, timeout time.Duration) *driveAccessChangedClient {
	if endpoint == "" {
		return nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &driveAccessChangedClient{
		endpoint: endpoint,
		token:    token,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *driveAccessChangedClient) send(ctx context.Context, event lifecycleEventEnvelope) error {
	var revoked memberRevokedPayload
	if err := json.Unmarshal(event.Payload, &revoked); err != nil {
		return fmt.Errorf("project: decode member_revoked payload for Drive hint: %w", err)
	}
	body, _ := json.Marshal(struct {
		EventID     string `json:"event_id"`
		SpaceID     string `json:"space_id"`
		ProjectID   string `json:"project_id"`
		SubjectUID  string `json:"subject_uid"`
		MemberEpoch int64  `json:"member_epoch"`
	}{
		EventID:     event.EventID,
		SpaceID:     event.SpaceID,
		ProjectID:   event.ProjectID,
		SubjectUID:  revoked.SubjectUID,
		MemberEpoch: revoked.MemberEpoch,
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("project: create Drive access hint request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(projectprovision.HeaderInternalToken, c.token)

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("project: send Drive access hint: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("project: Drive access hint returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (p *Project) sendDriveAccessChangedHint(row lifecycleEventRow) {
	if row.EventType != LifecycleEventMemberRevoked || p.driveAccessChangedClient == nil {
		return
	}
	if err := p.driveAccessChangedClient.send(context.Background(), row.envelope()); err != nil {
		p.Warn("发送 Drive 项目授权变化提示失败",
			zap.String("event_id", row.EventID), zap.Error(err))
	}
}
