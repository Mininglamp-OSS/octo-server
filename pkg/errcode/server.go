// Package errcode registers octo-server private business error codes.
package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrThreadGroupNoInvalid = register(codes.Code{
		ID:             "err.server.thread.group_no_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid group number.",
		SafeDetailKeys: []string{"field"},
	})
	ErrThreadShortIDInvalid = register(codes.Code{
		ID:             "err.server.thread.short_id_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid thread short ID.",
		SafeDetailKeys: []string{"field"},
	})
	ErrThreadRequestInvalid = register(codes.Code{
		ID:             "err.server.thread.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field", "max_size"},
	})
	ErrThreadNameInvalid = register(codes.Code{
		ID:             "err.server.thread.name_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Thread name is required and must not exceed 100 characters.",
		SafeDetailKeys: []string{"field", "max_length"},
	})
	ErrThreadSourceMessageInvalid = register(codes.Code{
		ID:             "err.server.thread.source_message_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid source message payload.",
		SafeDetailKeys: []string{"field", "max_size"},
	})
	ErrThreadStatusInvalid = register(codes.Code{
		ID:             "err.server.thread.status_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid thread status.",
		SafeDetailKeys: []string{"field"},
	})
	ErrThreadNotGroupMember = register(codes.Code{
		ID:             "err.server.thread.not_group_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a group member.",
	})
	ErrThreadPermissionDenied = register(codes.Code{
		ID:             "err.server.thread.permission_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to perform this action.",
	})
	ErrThreadNotFound = register(codes.Code{
		ID:             "err.server.thread.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Thread not found.",
	})
	ErrThreadDeleted = register(codes.Code{
		ID:             "err.server.thread.deleted",
		HTTPStatus:     http.StatusGone,
		DefaultMessage: "Thread has been deleted.",
	})
	ErrThreadNotActive = register(codes.Code{
		ID:             "err.server.thread.not_active",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Thread is not active.",
	})
	ErrThreadStatusChanged = register(codes.Code{
		ID:             "err.server.thread.status_changed",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Thread status changed concurrently.",
	})
	ErrThreadCreatorCannotLeave = register(codes.Code{
		ID:             "err.server.thread.creator_cannot_leave",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Thread creator cannot leave the thread.",
	})
	ErrThreadGroupMDNotFound = register(codes.Code{
		ID:             "err.server.thread.group_md_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Thread GROUP.md not found.",
	})
	ErrThreadGroupMDContentEmpty = register(codes.Code{
		ID:             "err.server.thread.group_md_content_empty",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "GROUP.md content must not be empty.",
		SafeDetailKeys: []string{"field"},
	})
	ErrThreadGroupMDContentTooLarge = register(codes.Code{
		ID:             "err.server.thread.group_md_content_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "GROUP.md content exceeds the maximum size.",
		SafeDetailKeys: []string{"field", "max_size"},
	})
	ErrThreadSettingInvalid = register(codes.Code{
		ID:             "err.server.thread.setting_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid thread setting.",
		SafeDetailKeys: []string{"field"},
	})
	ErrThreadStoreFailed = register(codes.Code{
		ID:             "err.server.thread.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Thread storage operation failed.",
		Internal:       true,
	})
)

func register(c codes.Code) codes.Code {
	codes.Register(c)
	return c
}
