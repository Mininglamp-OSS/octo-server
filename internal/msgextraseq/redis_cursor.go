package msgextraseq

import (
	"fmt"
	"strconv"

	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
	messageExtraCursorKeyPattern = "messageExtraVersion:*"
	redisCursorScanBatchSize     = int64(500)
)

type redisCursorEvidence struct {
	maxCursor  int64
	keyCount   int64
	fieldCount int64
}

// observeRedisCursorEvidence scans the persisted sync cursors without using
// KEYS or HGETALL. SCAN/HSCAN keep each Redis command bounded while still
// visiting the complete keyspace required for a safe cutover floor.
//
// The scan is intentionally operator-only (Preflight/Activate), never on a
// request path. It reports only aggregate counts and the maximum value; Redis
// keys and hash fields contain user/source/channel identifiers and must not be
// logged or printed.
func (s *Store) observeRedisCursorEvidence() (redisCursorEvidence, error) {
	client, err := s.newRedisScanClient()
	if err != nil {
		return redisCursorEvidence{}, err
	}
	defer client.Close()

	var evidence redisCursorEvidence
	seenKeys := make(map[string]struct{})
	var keyCursor uint64
	for {
		keys, next, err := client.Scan(keyCursor, messageExtraCursorKeyPattern, redisCursorScanBatchSize).Result()
		if err != nil {
			return redisCursorEvidence{}, fmt.Errorf("msgextraseq: scan Redis cursor keys: %w", err)
		}
		for _, key := range keys {
			// Redis SCAN may return a key more than once while the keyspace changes.
			// Avoid rescanning it and inflating the aggregate key/field counts.
			if _, ok := seenKeys[key]; ok {
				continue
			}
			seenKeys[key] = struct{}{}
			evidence.keyCount++

			var hashCursor uint64
			for {
				entries, hashNext, err := client.HScan(key, hashCursor, "*", redisCursorScanBatchSize).Result()
				if err != nil {
					return redisCursorEvidence{}, fmt.Errorf("msgextraseq: scan Redis cursor hash: %w", err)
				}
				if len(entries)%2 != 0 {
					return redisCursorEvidence{}, fmt.Errorf("msgextraseq: scan Redis cursor hash returned an odd entry count")
				}
				for i := 1; i < len(entries); i += 2 {
					cursor, err := strconv.ParseInt(entries[i], 10, 64)
					if err != nil {
						return redisCursorEvidence{}, fmt.Errorf("msgextraseq: parse Redis cursor value: %w", err)
					}
					evidence.fieldCount++
					if cursor > evidence.maxCursor {
						evidence.maxCursor = cursor
					}
				}
				hashCursor = hashNext
				if hashCursor == 0 {
					break
				}
			}
		}
		keyCursor = next
		if keyCursor == 0 {
			break
		}
	}
	return evidence, nil
}

func (s *Store) newRedisScanClient() (*rd.Client, error) {
	opts, err := octoredis.BuildOptions(s.ctx.GetConfig(), func(opts *rd.Options) {
		opts.MaxRetries = 3
		opts.PoolSize = 1
	})
	if err != nil {
		return nil, fmt.Errorf("msgextraseq: build Redis scan options: %w", err)
	}
	return octoredis.InstrumentedClientFromOptions(opts), nil
}
