package carddispatch

import (
	"errors"
	"fmt"
)

type Category string

const (
	CategoryOK                Category = "ok"
	CategoryInvalidRequest    Category = "invalid_request"
	CategoryFeatureDisabled   Category = "feature_disabled"
	CategoryProducerDisabled  Category = "producer_disabled"
	CategoryIdentityUntrusted Category = "identity_untrusted"
	CategoryTargetDenied      Category = "target_denied"
	CategoryCardInvalid       Category = "card_invalid"
	CategoryPayloadTooLarge   Category = "payload_too_large"
	CategoryBusy              Category = "busy"
	CategoryDispatchFailed    Category = "dispatch_failed"
)

var (
	ErrTargetDenied             = errors.New("carddispatch: target denied")
	ErrRegistryAlreadyInstalled = errors.New("carddispatch: registry already installed")
)

type Error struct {
	Category Category
	Cause    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		return "carddispatch: " + string(e.Category)
	}
	return fmt.Sprintf("carddispatch: %s: %v", e.Category, e.Cause)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func CategoryOf(err error) Category {
	if err == nil {
		return CategoryOK
	}
	var dispatchErr *Error
	if errors.As(err, &dispatchErr) {
		return dispatchErr.Category
	}
	return CategoryDispatchFailed
}

func categorized(category Category, cause error) error {
	return &Error{Category: category, Cause: cause}
}
