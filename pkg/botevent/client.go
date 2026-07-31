package botevent

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// Producers need a client that can EVAL, which octo-lib's *redis.Conn does not
// expose. Rather than thread a new dependency through every call site, the
// package owns one process-wide instrumented client.
//
// Ringing is short and non-blocking, so this pool stays small: unlike the
// waiting side (BLPOP pins a connection for the whole hold), a ring returns in
// one round trip. Sized like the other short-transaction clients in this repo
// (modules/user, modules/incomingwebhook, modules/file, modules/group all use
// an explicit PoolSize of 10).
const ringPoolSize = 10

var (
	ringClientOnce sync.Once
	ringClient     *rd.Client
)

// RingClient returns the shared doorbell client. Built through octoredis so TLS
// options are honoured and pkg/redis's raw-client chokepoint guard stays
// satisfied.
func RingClient(cfg *config.Config) Ringer {
	ringClientOnce.Do(func() {
		ringClient = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = 1
			o.PoolSize = ringPoolSize
		})
	})
	return ringClient
}
