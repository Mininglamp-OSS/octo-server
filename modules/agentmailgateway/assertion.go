package agentmailgateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	assertionPrefix  = "omg_"
	assertionVersion = 1
	assertionIssuer  = "octo-server"
)

var errInvalidAssertionInput = errors.New("invalid gateway assertion input")

// assertionClaims is the wire contract shared with octo-mail. It binds the
// authenticated OCTO identity to one exact upstream HTTP request.
type assertionClaims struct {
	Version    int    `json:"v"`
	Issuer     string `json:"iss"`
	Subject    string `json:"sub"`
	SpaceID    string `json:"space"`
	MailboxID  string `json:"mailbox,omitempty"`
	Method     string `json:"method"`
	RequestURI string `json:"uri"`
	BodySHA256 string `json:"body_sha256"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
	Nonce      string `json:"nonce"`
}

func signAssertion(secret []byte, subject, spaceID, method, requestURI string, body []byte, now time.Time, nonceSource io.Reader) (string, error) {
	return signAssertionForMailbox(secret, subject, spaceID, "", method, requestURI, body, now, nonceSource)
}

func signAssertionForMailbox(secret []byte, subject, spaceID, mailboxID, method, requestURI string, body []byte, now time.Time, nonceSource io.Reader) (string, error) {
	if len(secret) < 32 || strings.TrimSpace(subject) == "" || strings.TrimSpace(spaceID) == "" ||
		strings.TrimSpace(method) == "" || requestURI == "" || nonceSource == nil {
		return "", errInvalidAssertionInput
	}

	nonce := make([]byte, 16)
	if _, err := io.ReadFull(nonceSource, nonce); err != nil {
		return "", fmt.Errorf("generate assertion nonce: %w", err)
	}
	bodySum := sha256.Sum256(body)
	now = now.UTC()
	claims := assertionClaims{
		Version:    assertionVersion,
		Issuer:     assertionIssuer,
		Subject:    strings.TrimSpace(subject),
		SpaceID:    strings.TrimSpace(spaceID),
		MailboxID:  strings.TrimSpace(mailboxID),
		Method:     strings.ToUpper(strings.TrimSpace(method)),
		RequestURI: requestURI,
		BodySHA256: hex.EncodeToString(bodySum[:]),
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(time.Minute).Unix(),
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal assertion claims: %w", err)
	}
	payloadText := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payloadText))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return assertionPrefix + payloadText + "." + signature, nil
}

func randomNonceSource() io.Reader {
	return rand.Reader
}
