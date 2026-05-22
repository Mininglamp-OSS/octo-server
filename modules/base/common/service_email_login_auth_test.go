package common

import (
	"net/smtp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoginAuth_Start_RequiresTLS(t *testing.T) {
	auth := LoginAuth("user@example.com", "password", "smtp.example.com")

	// Non-TLS, non-localhost → must reject
	_, _, err := auth.(*loginAuth).Start(&smtp.ServerInfo{
		Name: "smtp.example.com",
		TLS:  false,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unencrypted")

	// TLS → must succeed
	proto, _, err := auth.(*loginAuth).Start(&smtp.ServerInfo{
		Name: "smtp.example.com",
		TLS:  true,
	})
	assert.NoError(t, err)
	assert.Equal(t, "LOGIN", proto)

	// localhost without TLS → must succeed (test carve-out)
	localAuth := LoginAuth("user@example.com", "password", "localhost")
	proto, _, err = localAuth.(*loginAuth).Start(&smtp.ServerInfo{
		Name: "localhost",
		TLS:  false,
	})
	assert.NoError(t, err)
	assert.Equal(t, "LOGIN", proto)
}

func TestLoginAuth_Start_RejectsWrongHost(t *testing.T) {
	auth := LoginAuth("user@example.com", "password", "smtp.example.com")

	_, _, err := auth.(*loginAuth).Start(&smtp.ServerInfo{
		Name: "evil.example.com",
		TLS:  true,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "wrong host")
}

func TestLoginAuth_Next(t *testing.T) {
	auth := LoginAuth("user@example.com", "s3cret", "smtp.example.com")
	la := auth.(*loginAuth)

	// Username challenge
	resp, err := la.Next([]byte("Username:"), true)
	assert.NoError(t, err)
	assert.Equal(t, []byte("user@example.com"), resp)

	// Password challenge
	resp, err = la.Next([]byte("Password:"), true)
	assert.NoError(t, err)
	assert.Equal(t, []byte("s3cret"), resp)

	// Case-insensitive / whitespace tolerance
	resp, err = la.Next([]byte("username: "), true)
	assert.NoError(t, err)
	assert.Equal(t, []byte("user@example.com"), resp)

	resp, err = la.Next([]byte("PASSWORD:"), true)
	assert.NoError(t, err)
	assert.Equal(t, []byte("s3cret"), resp)

	// Unknown challenge → error
	_, err = la.Next([]byte("OTP:"), true)
	assert.Error(t, err)

	// more=false → nil, nil
	resp, err = la.Next([]byte("anything"), false)
	assert.NoError(t, err)
	assert.Nil(t, resp)
}

func TestIsLocalhost(t *testing.T) {
	assert.True(t, isLocalhost("localhost"))
	assert.True(t, isLocalhost("127.0.0.1"))
	assert.True(t, isLocalhost("::1"))
	assert.False(t, isLocalhost("smtp.example.com"))
	assert.False(t, isLocalhost(""))
}
