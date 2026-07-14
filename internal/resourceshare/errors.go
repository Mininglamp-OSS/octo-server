package resourceshare

import "errors"

var (
	ErrProviderConfig   = errors.New("resource share provider configuration invalid")
	ErrProviderNotFound = errors.New("resource share provider not found")
	ErrProviderDisabled = errors.New("resource share provider disabled")
	ErrIntentInvalid    = errors.New("resource share intent invalid")
	ErrIntentSignature  = errors.New("resource share intent signature invalid")
	ErrIntentReplay     = errors.New("resource share intent replay conflict")
	ErrProofConfig      = errors.New("resource share proof configuration invalid")
	ErrProofMissing     = errors.New("resource share proof missing")
	ErrProofInvalid     = errors.New("resource share proof invalid")
	ErrTargetDenied     = errors.New("resource share target denied")
	ErrTargetQuery      = errors.New("resource share target query failed")
)
