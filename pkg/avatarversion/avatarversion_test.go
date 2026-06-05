package avatarversion

import (
	"sync"
	"testing"
)

func TestNewReturnsUniquePositiveVersionsUnderConcurrency(t *testing.T) {
	const goroutines = 32
	const perGoroutine = 2000

	versions := make(chan int64, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				versions <- New()
			}
		}()
	}
	wg.Wait()
	close(versions)

	seen := make(map[int64]struct{}, goroutines*perGoroutine)
	for version := range versions {
		if version <= 0 {
			t.Fatalf("New() returned non-positive version %d", version)
		}
		if _, ok := seen[version]; ok {
			t.Fatalf("New() returned duplicate version %d", version)
		}
		seen[version] = struct{}{}
	}
}
