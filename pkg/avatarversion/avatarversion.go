package avatarversion

import (
	"crypto/rand"
	"encoding/binary"
	"sync/atomic"
	"time"
)

const (
	randomBits = 20
	randomMask = (1 << randomBits) - 1
)

var lastVersion atomic.Int64

// New returns a positive object-key version for avatar uploads.
//
// The high bits preserve coarse time ordering, while the low random bits make
// same-millisecond cross-node collisions unlikely. The process-local CAS keeps
// versions unique and monotonic even if the clock stalls or crypto/rand fails.
func New() int64 {
	version := time.Now().UnixMilli()<<randomBits | randomSuffix()
	for {
		last := lastVersion.Load()
		if version <= last {
			version = last + 1
		}
		if lastVersion.CompareAndSwap(last, version) {
			return version
		}
	}
}

func randomSuffix() int64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return int64(binary.BigEndian.Uint64(buf[:]) & randomMask)
	}
	return time.Now().UnixNano() & randomMask
}
