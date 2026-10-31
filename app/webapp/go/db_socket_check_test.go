//go:build linux || darwin

package isureal

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

func TestPooledSocketLivenessProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if socketNeedsPing(client) || socketNeedsPing(tls.Client(client, &tls.Config{})) {
		t.Fatal("healthy idle TCP connection would pay an unnecessary protocol round trip")
	}
	waitForPing := func() {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for !socketNeedsPing(client) {
			if time.Now().After(deadline) {
				t.Fatal("pending bytes or peer shutdown not detected")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	waitForPing()
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(client, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("probe consumed data, read %q: %v", b, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	waitForPing()
}
