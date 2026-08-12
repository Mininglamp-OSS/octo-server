package auth

// RolloutBootOutcome is a low-cardinality startup classification. Redis is
// consulted only during the one-time legacy takeover; after the singleton
// exists every boot is normal regardless of Redis contents.
type RolloutBootOutcome string

const (
	RolloutBootFresh   RolloutBootOutcome = "fresh"
	RolloutBootAdopted RolloutBootOutcome = "adopted"
	RolloutBootNormal  RolloutBootOutcome = "normal"
)

type RolloutBoot struct {
	Outcome       RolloutBootOutcome
	Floor         SessionMode
	Mode          SessionMode
	MaxPerUID     int
	Version       int64
	Warning       string
	AutoAdvance   bool
	CanaryAhead   bool
	ExpectWriters int
}
