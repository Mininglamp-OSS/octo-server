// Package messagesql exposes the existing migration resources without importing
// the message module and its runtime dependencies.
package messagesql

import "embed"

// Files contains the original SQL files; migration text has one source of truth.
//
//go:embed *.sql
var Files embed.FS
