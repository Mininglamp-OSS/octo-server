package auth_jwt

import (
	"crypto/rand"
	"encoding/hex"
)

func defaultReadRand(b []byte) (int, error) { return rand.Read(b) }
func defaultHexEncode(b []byte) string      { return hex.EncodeToString(b) }
