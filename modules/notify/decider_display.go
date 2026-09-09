package notify

import (
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

var decisionDisplayLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return loc
}()

// deciderUserResolver resolves a decider's display name from their uid. The
// package's user.IService satisfies it; a test may inject a fake.
type deciderUserResolver interface {
	GetUser(uid string) (*user.Resp, error)
}

// deciderSpaceResolver verifies (spaceID, uid) is an ACTIVE membership and, when
// so, returns that Space's name (ok=true). ok=false means the pair is not an
// active membership — the caller must not infer any other Space.
type deciderSpaceResolver interface {
	ResolveActiveMemberSpaceName(spaceID, uid string) (string, bool, error)
}

// spaceMemberNameResolver adapts pkg/space to deciderSpaceResolver over a live
// DB session. It is the production implementation used by both the finalizer and
// the /v1/internal/cards/mutate handler.
type spaceMemberNameResolver struct{ session *dbr.Session }

func (r spaceMemberNameResolver) ResolveActiveMemberSpaceName(spaceID, uid string) (string, bool, error) {
	return spacepkg.ResolveActiveMemberSpaceName(r.session, spaceID, uid)
}

// resolvedDeciderDisplay is octo-server's server-authoritative resolution of a
// decision actor's display. Both fields are best-effort: an empty value means
// the lookup did not resolve, and the caller must degrade to a generic
// placeholder (docsLabelsFor(lang).decisionActor for the name) — never to a
// caller-supplied string, which is exactly what the authoritative ids replace.
type resolvedDeciderDisplay struct {
	OperatorName      string
	OperatorSpaceName string
}

// resolveDeciderDisplay resolves the decider's display name and operator Space
// name from the authoritative decider ids. The name comes from the user service;
// the operator Space name is resolved ONLY when deciderSpaceID is an active
// membership of that SAME uid — a card/document Space or a default Space is
// never inferred. Every lookup failure (error, miss, or non-membership) degrades
// the corresponding optional field to empty; this function never returns an
// error, so it can never fail an already-committed decision. warn, if non-nil,
// is invoked on a lookup ERROR (not on an ordinary miss) for observability.
func resolveDeciderDisplay(users deciderUserResolver, spaces deciderSpaceResolver, deciderUID, deciderSpaceID string, warn func(string, error)) resolvedDeciderDisplay {
	var out resolvedDeciderDisplay
	deciderUID = strings.TrimSpace(deciderUID)
	if deciderUID == "" {
		return out
	}
	if users != nil {
		if resp, err := users.GetUser(deciderUID); err != nil {
			if warn != nil {
				warn("resolve decider name", err)
			}
		} else if resp != nil {
			out.OperatorName = strings.TrimSpace(resp.Name)
		}
	}
	deciderSpaceID = strings.TrimSpace(deciderSpaceID)
	if deciderSpaceID != "" && spaces != nil {
		if name, ok, err := spaces.ResolveActiveMemberSpaceName(deciderSpaceID, deciderUID); err != nil {
			if warn != nil {
				warn("resolve decider space name", err)
			}
		} else if ok {
			out.OperatorSpaceName = strings.TrimSpace(name)
		}
		// ok=false → the decider is not an active member of that Space (or it is
		// inactive): leave OperatorSpaceName empty. Do NOT fall back to any other
		// Space.
	}
	return out
}

// formatDecisionTime renders authoritative unix seconds for the zh-CN card in
// an explicit product timezone. It never follows the container-local timezone.
func formatDecisionTime(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).In(decisionDisplayLocation).Format("2006-01-02 15:04")
}
