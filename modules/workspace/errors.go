package workspace

import "errors"

// Sentinel errors returned by the service. HTTP adapters map these to the
// registered localized error codes; wrapping is intentional so errors.Is works
// through database-operation context.
var (
	ErrRequestInvalid        = errors.New("workspace: request invalid")
	ErrSpaceRequired         = errors.New("workspace: space required")
	ErrForbidden             = errors.New("workspace: forbidden")
	ErrNotFound              = errors.New("workspace: not found")
	ErrOwnerProtected        = errors.New("workspace: owner protected")
	ErrRoleInvalid           = errors.New("workspace: workspace role invalid")
	ErrCandidateIneligible   = errors.New("workspace: candidate ineligible")
	ErrDependencyUnavailable = errors.New("workspace: dependency unavailable")
)
