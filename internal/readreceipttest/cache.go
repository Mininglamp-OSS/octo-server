// Package readreceipttest provides a small RESP capture server for receipt handler tests.
package readreceipttest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Cache counts SET commands without running the aggregation worker.
type Cache struct {
	Addr      string
	sets      atomic.Int32
	committed atomic.Bool
	early     atomic.Bool
	commands  chan []string
}

// NewCache starts a RESP endpoint and joins all its goroutines during cleanup.
func NewCache(t *testing.T) *Cache {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &Cache{Addr: listener.Addr().String(), commands: make(chan []string, 100)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var connections []net.Conn
	accepted := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
					if err != nil {
						return
					}
					args := make([]string, count)
					for i := range args {
						line, err = r.ReadString('\n')
						if err != nil {
							return
						}
						n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
						if err != nil {
							return
						}
						value := make([]byte, n+2)
						if _, err := io.ReadFull(r, value); err != nil {
							return
						}
						args[i] = string(value[:n])
					}
					if len(args) > 0 && strings.EqualFold(args[0], "set") {
						if !c.committed.Load() {
							c.early.Store(true)
						}
						c.sets.Add(1)
						c.commands <- args
					}
					if _, err := fmt.Fprint(conn, "+OK\r\n"); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-accepted
		mu.Lock()
		for _, conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return c
}

// WaitSet waits for the handler's cache write and returns its RESP arguments.
func (c *Cache) WaitSet(t *testing.T) []string {
	t.Helper()
	select {
	case args := <-c.commands:
		if c.early.Load() {
			t.Error("cache write preceded successful commit")
		}
		return args
	case <-time.After(time.Second):
		t.Fatal("no post-commit cache write")
		return nil
	}
}

// Count returns the number of cache SET commands observed.
func (c *Cache) Count() int { return int(c.sets.Load()) }

// AssertQuiet detects unexpected asynchronous writes, including a second pass.
func (c *Cache) AssertQuiet(t *testing.T) {
	t.Helper()
	select {
	case args := <-c.commands:
		t.Errorf("unexpected cache write: %v", args)
	case <-time.After(50 * time.Millisecond):
	}
}

// CommitEvents tracks the real DBR successful-commit event for cache ordering.
type CommitEvents struct {
	dbr.NullEventReceiver
	Cache *Cache
}

func (e *CommitEvents) Event(name string) {
	if name == "dbr.commit" {
		e.Cache.committed.Store(true)
	}
}
