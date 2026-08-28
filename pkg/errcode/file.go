package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	// ErrFileUploadTooLarge is returned by the main upload endpoints when a file
	// exceeds the operator-configured cap (system_setting file.max_size_kb).
	//
	// The cap is expressed in KB and can be any value, so the message carries a
	// pre-formatted, human-readable limit ("1.5 MB") rather than an integer
	// count of megabytes: the previous raw response divided bytes by 1024*1024,
	// which reported "1MB" for a 1536KB cap — a limit the server does not
	// actually enforce.
	//
	// max_size_kb is the exact value clients should branch on; max_mb is the
	// truncated legacy field, kept only for clients already reading it.
	ErrFileUploadTooLarge = register(codes.Code{
		ID:             "err.server.file.upload_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The file exceeds the maximum allowed size of {{.max_size}}.",
		SafeDetailKeys: []string{"max_size_kb", "max_mb"},
	})

	// ErrFileExtensionListTooLong is returned when the raw CSV exceeds the byte
	// bound, as opposed to the entry-count / per-entry-length bounds. Separate
	// from ErrFileExtensionListTooLarge so the operator is told what they
	// actually exceeded: 20 well-formed entries inside a 5000-byte payload
	// would otherwise be reported as a violation of bounds they did not hit.
	ErrFileExtensionListTooLong = register(codes.Code{
		ID:             "err.server.file.extension_list_too_long",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The extension list is too long (max {{.max_bytes}} bytes).",
		SafeDetailKeys: []string{"max_bytes", "got_bytes"},
	})

	// ErrFileExtensionListTooLarge is returned by the manager system_setting
	// write path when file.extra_{allowed,blocked}_extensions exceeds the entry
	// count or per-entry length bound.
	//
	// system_setting.value is TEXT (64KB) and the string write path is
	// "anything goes", so without a bound a single mis-configuration writes
	// thousands of extensions: every upload request rebuilds a map that size,
	// and — worse — the list is served verbatim from /v1/common/appconfig,
	// which is unauthenticated and polled by every client.
	//
	// The sticker keys never needed this: their read side intersects with a
	// fixed 5-entry raster allowlist, which bounds them by construction. The
	// file allowlist lost that bound when the candidate set was dropped.
	ErrFileExtensionListTooLarge = register(codes.Code{
		ID:             "err.server.file.extension_list_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The extension list is too large (max {{.max_entries}} entries, {{.max_length}} chars each).",
		SafeDetailKeys: []string{"max_entries", "got", "extension", "max_bytes", "got_bytes"},
	})
)
