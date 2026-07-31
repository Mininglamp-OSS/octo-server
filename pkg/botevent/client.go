package botevent

import (
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// Producers need a client that can EVAL, which octo-lib's *redis.Conn does not
// expose. Rather than thread a new dependency through every call site, the
// package owns one process-wide instrumented client.
//
// ringPoolSize is derived from ringWorkers, the only thing that uses this client
// concurrently — plus headroom for reconnects. That derivation is the point: an
// earlier revision set it to 10 by copying the convention of unrelated
// short-transaction clients (modules/user, modules/file, …) while the path it
// served admitted 100 concurrent callers, which is a designed-in 10:1 contention
// ratio dressed up as a convention. Sizing from the actual consumer means a
// worker never waits for a connection, so PoolTimeout cannot enter the ring's
// tail latency.
const ringPoolSize = ringWorkers + 2

// The ring is a hint on the hottest producer path, so it must give up long
// before it can matter to a message. Every timeout below is deliberately far
// tighter than go-redis's defaults (5s dial / 3s read / 4s pool timeout), which
// would let a degraded Redis cost seconds per ring.
//
// A Redis that cannot answer one EVALSHA within ringReadTimeout is in trouble;
// dropping the bell then costs a waiter at most one chunk (5s) of latency, which
// is strictly cheaper than making producers wait. MaxRetries is 0 for the same
// reason — a retried hint is a hint that took twice as long.
const (
	ringDialTimeout = 500 * time.Millisecond
	ringReadTimeout = 200 * time.Millisecond
	ringPoolTimeout = 100 * time.Millisecond
)

var (
	ringClientOnce sync.Once
	ringClient     *rd.Client
)

// RingClient returns the shared doorbell client. Built through octoredis so TLS
// options are honoured and pkg/redis's raw-client chokepoint guard stays
// satisfied.
//
// Producers should call Notify, not Ring with this client: the client's timeouts
// bound one ring, while Notify is what keeps the ring off the producer's
// goroutine entirely.
func RingClient(cfg *config.Config) Ringer {
	ringClientOnce.Do(func() {
		ringClient = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = 0
			o.PoolSize = ringPoolSize
			o.DialTimeout = ringDialTimeout
			o.ReadTimeout = ringReadTimeout
			o.WriteTimeout = ringReadTimeout
			o.PoolTimeout = ringPoolTimeout
		})
	})
	return ringClient
}
