package robot

import (
	"errors"
	"testing"
)

// TestVerifiedPayloadRobotIDRequiresPositiveVerification pins the trust boundary
// for client-controlled payload.robot_id values.
//
// The allocator creates a no-TTL counter for every adopted id. A lookup error is
// not positive evidence that the id names a bot, so returning the candidate in
// that case turns a transient dependency failure into unbounded permanent Redis
// state. The candidate may be adopted only after a successful existence check.
func TestVerifiedPayloadRobotIDRequiresPositiveVerification(t *testing.T) {
	lookupErr := errors.New("lookup failed")
	cases := []struct {
		name    string
		exists  bool
		err     error
		want    string
		wantErr error
	}{
		{name: "verified bot", exists: true, want: "known_bot"},
		{name: "known missing", exists: false},
		{name: "lookup failure", err: lookupErr, wantErr: lookupErr},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verifiedPayloadRobotID("known_bot", tc.exists, tc.err)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("robot id = %q, want %q", got, tc.want)
			}
		})
	}
}
