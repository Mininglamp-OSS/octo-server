package agentmailgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	gatewayProvisioningPath     = "/internal/v0/gateway-identities/ensure"
	maxProvisioningResponseSize = 8 << 10
)

type provisioningUserIdentity struct {
	Username string `db:"username"`
	ShortNo  string `db:"short_no"`
}

func (g *Gateway) ensureProvisioned(ctx context.Context, uid, spaceID string) error {
	key := uid + "\x00" + spaceID
	if g.provisioned != nil {
		if _, ok := g.provisioned.Get(key); ok {
			return nil
		}
	}
	if g.provisionIdentity == nil {
		return fmt.Errorf("gateway provisioning is not configured")
	}
	result := g.provisioningFlights.DoChan(key, func() (any, error) {
		if g.provisioned != nil {
			if _, ok := g.provisioned.Get(key); ok {
				return nil, nil
			}
		}
		provisionCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), normalizedGatewayTimeout(g.cfg.timeout),
		)
		defer cancel()
		if err := g.provisionIdentity(provisionCtx, uid, spaceID); err != nil {
			return nil, err
		}
		if g.provisioned != nil {
			g.provisioned.Add(key, struct{}{})
		}
		return nil, nil
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case completed := <-result:
		return completed.Err
	}
}

func (g *Gateway) provisionGatewayIdentity(ctx context.Context, uid, spaceID string) error {
	if g.resolveLocalpart == nil {
		return fmt.Errorf("gateway user identity resolver is unavailable")
	}
	localpart, err := g.resolveLocalpart(ctx, uid)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"localpart": localpart})
	if err != nil {
		return fmt.Errorf("marshal gateway provisioning request: %w", err)
	}
	target, err := g.internalURL(gatewayProvisioningPath)
	if err != nil {
		return err
	}
	token, err := signAssertion(
		g.cfg.secret, uid, spaceID, http.MethodPost, target.RequestURI(), body,
		g.now(), g.nonceSource,
	)
	if err != nil {
		return fmt.Errorf("sign gateway provisioning request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("construct gateway provisioning request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := g.client.Do(request)
	if err != nil {
		return fmt.Errorf("gateway provisioning request: %w", err)
	}
	response.Body = newIdleTimeoutReadCloser(response.Body, normalizedGatewayTimeout(g.cfg.timeout), nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxProvisioningResponseSize))
		return fmt.Errorf("gateway provisioning returned %s", response.Status)
	}
	written, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxProvisioningResponseSize+1))
	if err != nil {
		return fmt.Errorf("read gateway provisioning response: %w", err)
	}
	if written > maxProvisioningResponseSize {
		return fmt.Errorf("gateway provisioning response is too large")
	}
	return nil
}

func (g *Gateway) gatewayLocalpart(ctx context.Context, uid string) (string, error) {
	if g.ctx == nil || g.ctx.DB() == nil {
		return "", fmt.Errorf("gateway user directory is unavailable")
	}
	var identity provisioningUserIdentity
	if err := g.ctx.DB().Select(
		"IFNULL(username,'') AS username", "IFNULL(short_no,'') AS short_no",
	).From("`user`").Where(
		"uid=? AND status=1 AND IFNULL(is_destroy,0)<>2", uid,
	).LoadOneContext(ctx, &identity); err != nil {
		return "", fmt.Errorf("resolve gateway user identity: %w", err)
	}
	return normalizeGatewayLocalpart(identity.Username, identity.ShortNo, uid), nil
}

func normalizeGatewayLocalpart(candidates ...string) string {
	for _, candidate := range candidates {
		if normalized := sanitizeGatewayLocalpart(candidate); normalized != "" {
			return normalized
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(candidates, "\x00")))
	return "user-" + hex.EncodeToString(digest[:6])
}

func sanitizeGatewayLocalpart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(min(len(value), 64))
	lastDot := false
	var lastWritten byte
	for i := 0; i < len(value) && out.Len() < 64; i++ {
		ch := value[i]
		alphaNumeric := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
		if alphaNumeric {
			out.WriteByte(ch)
			lastWritten = ch
			lastDot = false
			continue
		}
		if out.Len() == 0 {
			continue
		}
		switch ch {
		case '.':
			if !lastDot {
				out.WriteByte(ch)
				lastWritten = ch
				lastDot = true
			}
		case '-', '_':
			out.WriteByte(ch)
			lastWritten = ch
			lastDot = false
		default:
			if lastWritten != '-' {
				out.WriteByte('-')
				lastWritten = '-'
			}
			lastDot = false
		}
	}
	return strings.TrimRight(out.String(), ".-_")
}

func (g *Gateway) internalURL(relativePath string) (*url.URL, error) {
	if g.cfg.upstream == nil || relativePath == "" || relativePath[0] != '/' {
		return nil, fmt.Errorf("missing gateway internal URL")
	}
	target := *g.cfg.upstream
	target.Path = strings.TrimRight(g.cfg.upstream.Path, "/") + relativePath
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return &target, nil
}
