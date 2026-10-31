//go:build linux || darwin

package isureal

import (
	"crypto/tls"
	"net"
	"syscall"
)

// A live, idle socket has no bytes ready. Peek without consuming protocol
// data; queued bytes (e.g. a final error), EOF and socket errors warrant a
// pgx ping. database/sql can then discard a bad connection before running SQL.
func socketNeedsPing(conn net.Conn) bool {
	if secure, ok := conn.(*tls.Conn); ok {
		conn = secure.NetConn()
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return true
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return true
	}
	var peekErr error
	err = raw.Control(func(fd uintptr) {
		var buf [1]byte
		_, _, peekErr = syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
	})
	return err != nil || (peekErr != syscall.EAGAIN && peekErr != syscall.EWOULDBLOCK)
}
