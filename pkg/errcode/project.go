package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.project.* — modules/project business error codes (api.go /
// api_member.go / middleware.go). DefaultMessage holds the en-US source (D4);
// the zh-CN runtime translation lives in pkg/i18n/locales/active.zh-CN.toml.
//
// Internal=true codes never surface their message on the wire — call sites MUST
// log the underlying err with full context (zap.Error) before responding.
//
// Anti-enumeration: ErrProjectNotFound is the single answer for "does not
// exist", "exists in a Space you are not in" and "exists but is unlisted and you
// are not a member". Splitting those apart would turn the route into an
// existence oracle for another tenant's projects; the specific reason goes to
// logs only. Same shape as modules/channel merging not-found with forbidden
// (modules/channel/api.go:179-194).
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrProjectRequestInvalid is the catch-all for missing / malformed request
	// input (BindJSON failure, empty required field, invalid enum). The offending
	// field is surfaced via Details when the caller can identify it.
	ErrProjectRequestInvalid = register(codes.Code{
		ID:             "err.server.project.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrProjectNameInvalid covers an empty or over-long project name. The cap is
	// surfaced so a client can render a localized hint without hard-coding it.
	ErrProjectNameInvalid = register(codes.Code{
		ID:             "err.server.project.name_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Project name is required and must not exceed the maximum length.",
		SafeDetailKeys: []string{"field", "max_chars"},
	})
	// ErrProjectFieldTooLong covers the description / logo length caps.
	ErrProjectFieldTooLong = register(codes.Code{
		ID:             "err.server.project.field_too_long",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The field exceeds the maximum allowed length.",
		SafeDetailKeys: []string{"field", "max_chars"},
	})
	// ErrProjectBatchTooLarge bounds the per-request member batch. A structural
	// cap on top of any byte cap, so a well-formed but pathological payload
	// cannot turn one request into an unbounded fan-out of membership writes.
	ErrProjectBatchTooLarge = register(codes.Code{
		ID:             "err.server.project.batch_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Too many members in a single request.",
		SafeDetailKeys: []string{"max"},
	})
	// ErrProjectAgentNotEligible is the SINGLE answer for every reason an AI
	// agent may not be seated in a project (D3/D15): it is not yours, it is not a
	// bot, it is self-hosted, it has no active Space seat, it is a system bot, it
	// does not exist, or its owner is not a member of this project.
	//
	// One code for seven reasons, deliberately. Splitting them apart turns
	// members/add into an oracle: submit a uid and learn whether it exists,
	// whether it is a bot, and who owns it. The specific reason goes to logs.
	//
	// Details carry the uids the CALLER submitted, so a client can highlight the
	// offending rows in its picker. Echoing back the caller's own input leaks
	// nothing — the leak would be saying which of them failed and why.
	ErrProjectAgentNotEligible = register(codes.Code{
		ID:             "err.server.project.agent_not_eligible",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "One or more selected AI agents cannot join this project.",
		SafeDetailKeys: []string{"uids"},
	})
	// ErrProjectRoleInvalid covers an out-of-range role value.
	ErrProjectRoleInvalid = register(codes.Code{
		ID:             "err.server.project.role_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid project member role.",
		SafeDetailKeys: []string{"field"},
	})
	ErrProjectCollaborationRoleNameInvalid = register(codes.Code{
		ID:             "err.server.project.collaboration_role_name_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The collaboration role name is invalid.",
		SafeDetailKeys: []string{"field", "max_chars"},
	})
	ErrProjectCollaborationRoleInvalid = register(codes.Code{
		ID:             "err.server.project.collaboration_role_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The collaboration role is invalid.",
	})
	ErrProjectCollaborationRoleTargetInvalid = register(codes.Code{
		ID:             "err.server.project.collaboration_role_target_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The collaboration role target is invalid.",
	})

	// ---- permission / policy (403) -------------------------------------------

	// ErrProjectPermissionDenied covers the project-level authorization guards,
	// including the transitive-protection rules (an admin may not remove or
	// demote another admin or the owner).
	ErrProjectPermissionDenied = register(codes.Code{
		ID:             "err.server.project.permission_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to perform this operation.",
	})
	// ErrProjectNotMember covers "you are not a member of this project" on a
	// route whose payload is members-only (e.g. the member roster).
	ErrProjectNotMember = register(codes.Code{
		ID:             "err.server.project.not_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this project.",
	})
	// ErrProjectDisabled covers the fail-closed project_create_enabled switch.
	// Every write path (create / update / disband / member write) returns it when
	// the flag is off; reads keep working so existing data stays observable.
	ErrProjectDisabled = register(codes.Code{
		ID:             "err.server.project.disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The project feature has been disabled by the administrator.",
	})
	ErrProjectCollaborationRoleDisabled = register(codes.Code{
		ID:             "err.server.project.collaboration_role_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Project collaboration-role changes are disabled.",
	})
	// ErrProjectMemberNotSpaceMember is the I1 rejection: the TARGET uid is not
	// an active member of the project's Space. Checked inside the request
	// transaction, so a non-member can never be admitted.
	ErrProjectMemberNotSpaceMember = register(codes.Code{
		ID:             "err.server.project.member_not_space_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The target user is not an active member of this space.",
	})
	// ErrProjectActorNotSpaceMember is the same rejection about the CALLER: their own
	// Space seat is gone (removed, or the Space went inactive), while their project seat
	// is still open because the Space-removal cascade is asynchronous.
	//
	// Separate from the target-level code above because the two ask the client to do
	// opposite things, and the message is the whole point: reusing the target-level code
	// told a caller whose own seat had closed that "the target user" was at fault, which
	// is exactly the misdirection this classification was added to remove (PR #841 round 2,
	// Jerry-Xin N-1). The create path was already reporting the creator's own missing seat
	// through the target-level code; it now uses this one.
	ErrProjectActorNotSpaceMember = register(codes.Code{
		ID:             "err.server.project.actor_not_space_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are no longer an active member of this space.",
	})

	// ---- quotas (403) --------------------------------------------------------

	// ErrProjectQuotaPinned covers the per-Space cap on pinned projects.
	//
	// A quota rather than an eviction: silently unpinning the oldest to make room
	// would be data loss the caller did not ask for and cannot see, and "why did my
	// pin disappear" is a support ticket nobody can answer from logs.
	//
	// Carries max so the client can say WHICH limit was hit without hardcoding the
	// number and drifting from the server.
	ErrProjectQuotaPinned = register(codes.Code{
		ID:             "err.server.project.quota_pinned",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You have pinned the maximum number of projects.",
		SafeDetailKeys: []string{"max"},
	})

	// ErrProjectQuotaPerSpace covers the per-Space project cap.
	ErrProjectQuotaPerSpace = register(codes.Code{
		ID:             "err.server.project.quota_per_space",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This space has reached its project limit.",
		SafeDetailKeys: []string{"max"},
	})
	// ErrProjectQuotaPerCreator covers the per-creator-per-Space project cap.
	ErrProjectQuotaPerCreator = register(codes.Code{
		ID:             "err.server.project.quota_per_creator",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You have reached your project limit in this space.",
		SafeDetailKeys: []string{"max"},
	})
	// ErrProjectQuotaMembers covers the per-project member cap.
	//
	// P0 deliberately did NOT register this: the cap was reachable only inside a batch
	// add, where it surfaces as a per-uid "quota_members" reason in a 200 partial report
	// (the other uids in the batch may well be admissible), and a registered code with no
	// producer costs an en-US marker and a zh-CN entry maintained forever. That comment
	// said it would be registered "when a caller needs it, not before".
	//
	// P2 is that caller. Creating a project with agent_uids seats N+1 members in ONE
	// transaction and refuses the WHOLE request if they do not fit (D3), so there is no
	// partial report to carry a per-uid reason — the refusal has to be an error code.
	// Without it the create falls to the Internal store_failed code: a 5xx, with the
	// message hidden, for what is an ordinary caller error with a number the client
	// should show.
	//
	// The batch-add path is unchanged and still reports per-uid.
	ErrProjectQuotaMembers = register(codes.Code{
		ID:             "err.server.project.quota_members",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This project has reached its member limit.",
		SafeDetailKeys: []string{"max"},
	})
	ErrProjectQuotaCollaborationRoles = register(codes.Code{
		ID:             "err.server.project.quota_collaboration_roles",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This project has reached its collaboration role limit.",
		SafeDetailKeys: []string{"max"},
	})
	ErrProjectQuotaMemberCollaborationRoles = register(codes.Code{
		ID:             "err.server.project.quota_member_collaboration_roles",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This member has reached the collaboration role limit.",
		SafeDetailKeys: []string{"max"},
	})
	// ErrProjectQuotaDailyCreate covers the per-user per-day creation cap.
	ErrProjectQuotaDailyCreate = register(codes.Code{
		ID:             "err.server.project.quota_daily_create",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You have created too many projects today.",
		SafeDetailKeys: []string{"max"},
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrProjectNotFound is the single anti-enumeration answer — see the package
	// comment above. It carries no Details, so the body is byte-identical
	// whichever of the three reasons produced it.
	ErrProjectNotFound = register(codes.Code{
		ID:             "err.server.project.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Project not found.",
	})
	// ErrProjectMemberNotFound covers a missing / already-removed target member.
	ErrProjectMemberNotFound = register(codes.Code{
		ID:             "err.server.project.member_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Project member not found.",
	})

	// ---- conflict (409) ------------------------------------------------------

	// ErrProjectNameDuplicated covers a duplicate ACTIVE name in the same Space.
	// A disbanded project frees its name (see the active_name generated column),
	// so this only fires against a live sibling.
	ErrProjectNameDuplicated = register(codes.Code{
		ID:             "err.server.project.name_duplicated",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "A project with this name already exists in this space.",
	})
	ErrProjectCollaborationRoleDuplicated = register(codes.Code{
		ID:             "err.server.project.collaboration_role_duplicated",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "A collaboration role with this name already exists in the project.",
	})
	// ErrProjectLastOwnerMustTransfer covers the last owner leaving or being
	// demoted without naming a successor. The transfer and the departure happen
	// in one transaction, so there is never a window with zero owners.
	ErrProjectLastOwnerMustTransfer = register(codes.Code{
		ID:             "err.server.project.last_owner_must_transfer",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Transfer ownership before leaving or being demoted.",
	})

	// ---- internal (500) ------------------------------------------------------

	// ErrProjectQueryFailed is the read-path storage failure. Internal=true, so
	// the renderer hides the message and details; log the cause first.
	ErrProjectQueryFailed = register(codes.Code{
		ID:             "err.server.project.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query project data.",
		Internal:       true,
	})
	// ErrProjectStoreFailed is the write-path storage failure. Internal=true.
	ErrProjectStoreFailed = register(codes.Code{
		ID:             "err.server.project.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to store project data.",
		Internal:       true,
	})
)
