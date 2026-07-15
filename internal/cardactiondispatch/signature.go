package cardactiondispatch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const signatureVersion = "v1"

func CanonicalRequest(method, path, timestamp, eventID string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		signatureVersion,
		strings.ToUpper(method),
		path,
		timestamp,
		eventID,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

func Sign(secret, method, path, timestamp, eventID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(CanonicalRequest(method, path, timestamp, eventID, body)))
	return signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

func Verify(secret, signature, method, path, timestamp, eventID string, body []byte) bool {
	want := Sign(secret, method, path, timestamp, eventID, body)
	return hmac.Equal([]byte(signature), []byte(want))
}
