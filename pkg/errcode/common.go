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
	// ErrFileUploadSizeOrdering is returned by the manager system_setting write
	// path when the prospective (merged) configuration would put the global
	// file upload cap BELOW the sticker upload cap.
	//
	// The upload handler checks the global cap first and the sticker cap second
	// (modules/file/api.go), so a global cap under the sticker cap makes sticker
	// uploads impossible while both keys look individually valid — the operator
	// raises the sticker cap, sees no effect, and gets no error anywhere.
	// Rejecting the write is the only place that failure mode is observable.
	//
	// Both caps ride along as details so the admin UI can render the conflict
	// without a second round-trip; neither is sensitive.
	ErrFileUploadSizeOrdering = register(codes.Code{
		ID:             "err.server.common.file_upload_size_ordering",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The global file upload size cap must not be smaller than the sticker upload cap.",
		SafeDetailKeys: []string{"file_max_size_kb", "sticker_max_size_kb"},
	})
	// ErrFileExtensionNotAllowlistable is returned by the manager system_setting
	// write path when file.extra_allowed_extensions contains an extension that
	// sits on the built-in blocklist (executables / scripts).
	//
	// That blocklist is deliberately not revocable through configuration, so
	// such a write would be silently inert: the operator sees the value stored,
	// and uploads keep getting rejected with no explanation anywhere. Rejecting
	// at write time is the only place the operator learns why.
	//
	// The offending extension rides along as a detail so the admin UI can point
	// at it; it is operator-supplied and not sensitive.
	ErrFileExtensionNotAllowlistable = register(codes.Code{
		ID:             "err.server.common.file_extension_not_allowlistable",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "This file extension is on the built-in blocklist and cannot be allowed.",
		SafeDetailKeys: []string{"extension"},
	})
	ErrManagerMFASmtpInvalid = register(codes.Code{
		ID:             "err.server.common.manager_mfa_smtp_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The SMTP configuration required by management-console MFA is invalid.",
	})
)
