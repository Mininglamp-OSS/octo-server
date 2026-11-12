package readreceipttest

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestCaptureDoesNotBlockWhenNotificationsFull(t *testing.T) {
	cache := NewCache(t)
	conn, err := net.Dial("tcp", cache.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for i := 0; i < 250; i++ {
		if _, err := fmt.Fprint(conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"); err != nil {
			t.Fatal(err)
		}
		if reply, err := reader.ReadString('\n'); err != nil || reply != "+OK\r\n" {
			t.Fatalf("SET %d: reply=%q err=%v", i, reply, err)
		}
	}
	if cache.Count() != 250 {
		t.Fatalf("lost command count: %d", cache.Count())
	}
}
