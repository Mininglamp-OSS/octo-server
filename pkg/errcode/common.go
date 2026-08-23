package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	// ErrSpaceWelcomeConfigInvalid is returned by the manager system_setting
	// write path when the onboarding space-welcome five-tuple does not form a
	// valid *enabled* combination — a missing/dissolved target Space, an
	// unparseable RFC3339 active_from, or a message body that is empty (after
	// trim) or exceeds the code-point limit. The `field` detail names the first
	// offending key so the admin UI can point at it; the specific reason stays
	// generic on the wire (log carries the rest).
	ErrSpaceWelcomeConfigInvalid = register(codes.Code{
		ID:             "err.server.common.space_welcome_config_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid space welcome configuration.",
		SafeDetailKeys: []string{"field"},
	})

	// ErrThreadArchiveWindowOrdering is returned by the manager system_setting
	// write path when the prospective (merged) configuration would put the
	// thread auto-archive window BELOW the sidebar recent-tab thread window.
	//
	// The two windows form a two-stage decay and only make sense in that order:
	// the per-viewer window fades a quiet thread out of the recent list first,
	// the global archive window later declares the topic finished. Inverting
	// them makes the recent-tab window unobservable — archiving removes the
	// thread from every list before its window elapses, so an operator who
	// widens the recent window sees no effect and gets no error. Rejecting the
	// write is the only place that failure mode can be surfaced.
	//
	// Both day counts ride along as details so the admin UI can render the
	// conflict without a second round-trip; neither is sensitive.
	ErrThreadArchiveWindowOrdering = register(codes.Code{
		ID:             "err.server.common.thread_archive_window_ordering",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Thread auto-archive window must not be shorter than the recent-tab thread window.",
		SafeDetailKeys: []string{"archive_days", "recent_days"},
	})

	// ErrManagerLogin2FAEmailUnconfigured rejects an attempt to turn
	// login.manager_2fa_on ON while at least one account that can sign in to the
	// management console (auth.IsManagerConsoleRole, enabled and not destroyed)
	// still has no email address.
	//
	// Without this guard the switch is a self-inflicted lockout: manager 2FA
	// fails closed at sign-in, so flipping it before the addresses exist locks
	// every affected administrator out of the very console needed to flip it
	// back. Validating at write time is the only place that failure mode can be
	// surfaced while the operator is still signed in.
	//
	// The offending usernames ride along as details — the endpoint is
	// SuperAdmin-only and the same names are already readable via
	// GET /v1/manager/user/admin, so this discloses nothing new and saves a
	// round-trip.
	ErrManagerLogin2FAEmailUnconfigured = register(codes.Code{
		ID:             "err.server.common.manager_2fa_email_unconfigured",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Cannot enable manager two-factor authentication: some console accounts have no email address configured.",
		SafeDetailKeys: []string{"accounts", "count"},
	})
)
