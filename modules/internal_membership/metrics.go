package internal_membership

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// configured reports whether the capability's token resolved at boot, i.e. whether
// these endpoints can answer anything at all.
//
// Why a gauge for a boolean that never changes after boot: the endpoints answer
// 401 both when the token is WRONG and when it is UNSET, deliberately — telling
// them apart lets an unauthenticated caller probe deployment state. The cost of
// that choice is that a peer holding a 401 cannot tell "my credential is wrong,
// stop retrying" from "the server is not configured yet, keep retrying", and the
// operator-side answer used to live only in a startup log line.
//
// A log line is not a durable answer here: this cluster keeps pod logs for a few
// minutes, and the env arrives through a ConfigMap that needs a restart to take
// effect — so "did the last rollout actually carry the token" is a question that
// gets asked long after the line is gone. A gauge answers it at any time.
//
// No labels: the env NAME is a constant and the value must never be exposed. It is
// on the internal /metrics endpoint, which is not the enumeration surface the 401
// choice was protecting — that surface is the public route, and it stays
// indistinguishable.
var configured = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "octo",
	Subsystem: "internal_membership",
	Name:      "configured",
	Help: "1 when OCTO_MEMBERSHIP_INTERNAL_TOKEN resolved at boot and the membership " +
		"internal endpoints can authenticate a caller; 0 when it is unset, too short, or " +
		"collides with a sibling capability's token, in which case every request is refused " +
		"with the same 401 a wrong token gets.",
})

// peerMaxCacheAge publishes the peer-side cache bound the operator declared when
// enabling this capability (PeerMaxCacheAgeEnv), in seconds. 0 means no bound is in
// force, which is also the value when the capability is off.
//
// It exists so the enablement gate is answerable from monitoring rather than from
// somebody's memory of a PR discussion. The account axis — a ban or a destroy moves
// no member_epoch — is bounded on the CONSUMER's side and by nothing here, so "what
// bound did we actually agree, and is it still what is deployed" is an operational
// question with no other answer.
//
// Publishing the number rather than a 0/1 flag is the same choice the env makes: a
// flag would say the box was ticked, and the value is the contract.
var peerMaxCacheAge = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "octo",
	Subsystem: "internal_membership",
	Name:      "peer_max_cache_age_seconds",
	Help: "The time bound, in seconds, that the peer is declared to place on decisions it " +
		"caches from these endpoints (OCTO_MEMBERSHIP_INTERNAL_PEER_MAX_CACHE_AGE_SECONDS). " +
		"0 when the capability is disabled or the declaration was rejected. This is the " +
		"worst-case window in which a banned or destroyed account keeps an authorization it " +
		"no longer holds, because member_epoch is per project and an account ban is per user.",
})
