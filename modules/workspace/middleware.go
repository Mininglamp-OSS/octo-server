package workspace

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// SpaceResolver resolves the authoritative Space for a request. Resource
// adapters use the resource identifier (rather than a caller-provided header)
// to implement the tenant boundary.
type SpaceResolver func(*wkhttp.Context) (string, error)

// VerifiedSpaceMiddleware verifies the resolved Space against the current
// session and publishes it through the shared Space context key. It deliberately
// performs a live CheckMembership read rather than maintaining a second positive
// authorization cache. The optional X-Space-Id header is an assertion only: it
// must agree with the resolved Space and can never select one.
//
// The resolver belongs to the owning module. This keeps Workspace independent
// from group (and from internal HTTP packages), while allowing group endpoints
// to reuse exactly the same authenticated Space gate.
func VerifiedSpaceMiddleware(ctx *config.Context, resolve SpaceResolver) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if ctx == nil || resolve == nil {
			RespondError(c, ErrDependencyUnavailable)
			c.Abort()
			return
		}

		uid := c.GetLoginUID()
		if uid == "" {
			// AuthMiddleware is mounted before this adapter. An empty UID here is
			// therefore a broken authentication context, not a valid anonymous
			// request; fail closed without consulting a caller-controlled field.
			RespondError(c, ErrForbidden)
			c.Abort()
			return
		}

		spaceID, err := resolve(c)
		if err != nil {
			switch {
			case errors.Is(err, ErrRequestInvalid),
				errors.Is(err, ErrSpaceRequired),
				errors.Is(err, ErrForbidden),
				errors.Is(err, ErrNotFound),
				errors.Is(err, ErrOwnerProtected),
				errors.Is(err, ErrRoleInvalid),
				errors.Is(err, ErrCandidateIneligible),
				errors.Is(err, ErrDependencyUnavailable):
			default:
				log.Error("resolve Workspace Space failed", zap.Error(err), zap.String("uid", uid))
				err = ErrDependencyUnavailable
			}
			RespondError(c, err)
			c.Abort()
			return
		}
		spaceID = strings.TrimSpace(spaceID)
		if spaceID == "" {
			RespondError(c, ErrSpaceRequired)
			c.Abort()
			return
		}

		// Check membership before examining the optional assertion. Otherwise
		// a foreign-but-real resource with a conflicting header would return a
		// different code from an unknown resource and become an existence oracle.
		member, err := spacepkg.CheckMembership(ctx.DB(), spaceID, uid)
		if err != nil {
			log.Error("verify Space membership failed", zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", uid))
			RespondError(c, ErrDependencyUnavailable)
			c.Abort()
			return
		}
		if !member {
			RespondError(c, ErrNotFound)
			c.Abort()
			return
		}

		if asserted := strings.TrimSpace(c.GetHeader("X-Space-Id")); asserted != "" && asserted != spaceID {
			RespondError(c, ErrSpaceRequired)
			c.Abort()
			return
		}

		spacepkg.SetSpaceID(c, spaceID)
		c.Next()
	}
}

// RequestScope creates the frozen service scope for a request. ExpectedSpaceID
// carries only the optional X-Space-Id consistency assertion; the verified
// context published by the middleware is intentionally not used as an authority
// substitute for a resource method.
func RequestScope(c *wkhttp.Context) Scope {
	if c == nil {
		return Scope{}
	}
	return Scope{
		UID:             c.GetLoginUID(),
		ExpectedSpaceID: strings.TrimSpace(c.GetHeader("X-Space-Id")),
	}
}

// ParsePage parses the common page_index/page_size query contract. Invalid,
// missing, and non-positive values use the documented defaults. There is no
// arbitrary maximum page index; values that do not fit the machine int are
// saturated at MaxInt so a service offset calculation cannot wrap negative.
func ParsePage(c *wkhttp.Context) Page {
	const (
		defaultIndex = 1
		defaultSize  = 15
		maxSize      = 200
	)
	if c == nil {
		return Page{Index: defaultIndex, Size: defaultSize}
	}

	index := parsePositiveInt(c.Query("page_index"), defaultIndex)
	size := parsePositiveInt(c.Query("page_size"), defaultSize)
	if size > maxSize {
		size = maxSize
	}
	return Page{Index: index, Size: size}
}

func parsePositiveInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		if raw[0] != '-' && errors.Is(err, strconv.ErrRange) {
			return int(^uint(0) >> 1)
		}
		return fallback
	}
	if n == 0 {
		return fallback
	}
	maxInt := uint64(^uint(0) >> 1)
	if n > maxInt {
		return int(maxInt)
	}
	return int(n)
}
