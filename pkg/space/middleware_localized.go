package space

// PR-C D5 — a localized twin of SpaceMiddleware.
//
// SpaceMiddleware answers with raw AbortWithStatusJSON and hardcoded Chinese
// strings. Every endpoint mounted on it today has clients that depend on those
// exact shapes, so it cannot be changed in place; but a new endpoint must not
// inherit them either, because the rest of the card-catalog surface answers
// through the i18n error envelope and a caller should not have to parse two
// error formats from one request chain.
//
// This variant is deliberately a twin, not a refactor: identical membership
// rule, identical cache keys and TTLs, identical "no space_id means skip".
// Only the failure responses differ. Keeping the logic duplicated for one
// release is cheaper than migrating every legacy route's wire contract as a
// side effect of adding two read endpoints — and the duplication is small,
// visible, and covered by tests on both sides.

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// LocalizedSpaceMiddleware validates the requested Space the same way
// SpaceMiddleware does, but reports failures through the localized envelope.
//
// An absent space_id is not an error: it means "no Space context", and the
// handler downstream is responsible for showing only what is visible without
// one. That is how a caller outside any Space still gets the public catalog.
func LocalizedSpaceMiddleware(ctx *config.Context) wkhttp.HandlerFunc {
	cache := NewRedisMembershipCache(ctx.GetRedisConn())
	return localizedSpaceMiddleware(func(spaceID, uid string) (bool, error) {
		return CheckMembership(ctx.DB(), spaceID, uid)
	}, cache)
}

func localizedSpaceMiddleware(check MembershipChecker, cache MembershipCache) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		spaceID := c.Query("space_id")
		if spaceID == "" {
			spaceID = c.GetHeader("X-Space-ID")
		}
		if spaceID == "" {
			c.Next()
			return
		}

		uid := c.GetLoginUID()
		if uid == "" {
			httperr.ResponseErrorL(c, errcode.ErrSharedAuthRequired, nil, nil)
			c.Abort()
			return
		}

		if isMember, found := cache.Get(spaceID, uid); found {
			if !isMember {
				httperr.ResponseErrorL(c, errcode.ErrSharedForbidden, nil, nil)
				c.Abort()
				return
			}
			SetSpaceID(c, spaceID)
			c.Next()
			return
		}

		isMember, err := check(spaceID, uid)
		if err != nil {
			// A membership check that failed is not a membership check that
			// said no: answering "forbidden" here would train callers to retry
			// with a different Space, and answering "allowed" would be a
			// tenant-isolation hole. Report the outage.
			//
			// Logged as well as answered, because the two twins report an outage
			// at different wire statuses (review P2-4, yujiawei). SpaceMiddleware
			// answers a wire 500; ResponseErrorL pins this one to 400 for D14
			// compatibility and carries the real status inside the envelope. A
			// MySQL or Redis failure inside CheckMembership would therefore be
			// invisible to 5xx-based alerting on these routes, so the log line is
			// the signal. Changing the wire status instead would need the D14
			// sign-off that ResponseErrorLWithStatus is reserved for.
			log.Error("localized space middleware: membership check failed",
				zap.String("space_id", spaceID), zap.String("uid", uid), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}

		ttl := cacheTTL
		if !isMember {
			ttl = negativeCacheTTL
		}
		cache.Set(spaceID, uid, isMember, ttl)

		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrSharedForbidden, nil, nil)
			c.Abort()
			return
		}

		SetSpaceID(c, spaceID)
		c.Next()
	}
}
