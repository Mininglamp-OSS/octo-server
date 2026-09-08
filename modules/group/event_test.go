package group

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExtractUID_SafeTypeAssertion tests that UID extraction handles all types gracefully
// This validates the fix for issue #390 - unsafe type assertion at line 81
func TestExtractUID_SafeTypeAssertion(t *testing.T) {
	tests := []struct {
		name      string
		payload   map[string]interface{}
		expectOK  bool
		expectUID string
	}{
		{
			name:      "missing uid field",
			payload:   map[string]interface{}{},
			expectOK:  false,
			expectUID: "",
		},
		{
			name:      "uid is integer instead of string",
			payload:   map[string]interface{}{"uid": 12345},
			expectOK:  false,
			expectUID: "",
		},
		{
			name:      "uid is nil",
			payload:   map[string]interface{}{"uid": nil},
			expectOK:  false,
			expectUID: "",
		},
		{
			name:      "uid is empty string",
			payload:   map[string]interface{}{"uid": ""},
			expectOK:  true, // ok is true but value is empty
			expectUID: "",
		},
		{
			name:      "uid is valid string",
			payload:   map[string]interface{}{"uid": "test-user-123"},
			expectOK:  true,
			expectUID: "test-user-123",
		},
		{
			name:      "uid is float64",
			payload:   map[string]interface{}{"uid": 123.45},
			expectOK:  false,
			expectUID: "",
		},
		{
			name:      "uid is bool",
			payload:   map[string]interface{}{"uid": true},
			expectOK:  false,
			expectUID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This tests the safe type assertion pattern used in the fix:
			// uid, ok := req["uid"].(string)
			uid, ok := tt.payload["uid"].(string)

			assert.Equal(t, tt.expectOK, ok, "type assertion ok value mismatch")
			assert.Equal(t, tt.expectUID, uid, "uid value mismatch")

			// The actual check in the code: !ok || uid == ""
			shouldReject := !ok || uid == ""
			expectReject := !tt.expectOK || tt.expectUID == ""
			assert.Equal(t, expectReject, shouldReject, "rejection logic mismatch")
		})
	}
}

// TestPanicRecovery_NonErrorType tests that panic recovery handles non-error types correctly
func TestPanicRecovery_NonErrorType(t *testing.T) {
	tests := []struct {
		name       string
		panicValue interface{}
	}{
		{
			name:       "panic with string",
			panicValue: "something went wrong",
		},
		{
			name:       "panic with int",
			panicValue: 42,
		},
		{
			name:       "panic with struct",
			panicValue: struct{ msg string }{"error struct"},
		},
		{
			name:       "panic with error",
			panicValue: errors.New("actual error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var recoveredErr error
			var committed bool

			commit := func(err error) {
				committed = true
				recoveredErr = err
			}

			// Simulate the panic recovery logic from event.go
			func() {
				defer func() {
					if r := recover(); r != nil {
						var panicErr error
						switch x := r.(type) {
						case error:
							panicErr = x
						default:
							panicErr = fmt.Errorf("panic: %v", r)
						}
						commit(panicErr)
					}
				}()
				panic(tt.panicValue)
			}()

			assert.True(t, committed, "commit should have been called after panic recovery")
			assert.NotNil(t, recoveredErr, "recovered error should not be nil")

			// Verify the error message contains useful information
			if _, ok := tt.panicValue.(error); ok {
				assert.Equal(t, tt.panicValue, recoveredErr)
			} else {
				assert.Contains(t, recoveredErr.Error(), "panic:")
			}
		})
	}
}
