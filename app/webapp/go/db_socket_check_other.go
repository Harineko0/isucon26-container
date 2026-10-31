//go:build !linux && !darwin

package isureal

import "net"

// On other platforms ask pgx to verify pooled connections with a protocol ping.
func socketNeedsPing(net.Conn) bool { return true }
