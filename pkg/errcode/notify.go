package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var ErrNotifyCardNotAllowed = register(codes.Code{
	ID:             "err.server.notify.card_not_allowed",
	HTTPStatus:     http.StatusBadRequest,
	DefaultMessage: "Card payloads are not allowed on the internal notification endpoint.",
})

var ErrNotifyCardInvalid = register(codes.Code{
	ID:             "err.server.notify.card_invalid",
	HTTPStatus:     http.StatusBadRequest,
	DefaultMessage: "The card notification request is invalid.",
})

// ErrNotifyCardMutateInvalid — the card-mutate request is malformed (missing
// locator, unsupported kind, or an invalid content edit).
var ErrNotifyCardMutateInvalid = register(codes.Code{
	ID:             "err.server.notify.card_mutate_invalid",
	HTTPStatus:     http.StatusBadRequest,
	DefaultMessage: "The card mutation request is invalid.",
})

// ErrNotifyCardMutateFailed — the target card could not be mutated (gone,
// sender mismatch, stale, or a backend error). The caller retries or falls back.
var ErrNotifyCardMutateFailed = register(codes.Code{
	ID:             "err.server.notify.card_mutate_failed",
	HTTPStatus:     http.StatusConflict,
	DefaultMessage: "The card could not be mutated.",
})

// ErrNotifyCardMutateForbidden — the target card is not a docs access card, so
// the docs-capability mutate endpoint refuses to touch it. The shared
// notification bot sends several producers' cards, so the target's own variant
// (not the sender identity) is the authoritative docs-domain gate.
var ErrNotifyCardMutateForbidden = register(codes.Code{
	ID:             "err.server.notify.card_mutate_forbidden",
	HTTPStatus:     http.StatusForbidden,
	DefaultMessage: "The target card cannot be mutated by this capability.",
})
