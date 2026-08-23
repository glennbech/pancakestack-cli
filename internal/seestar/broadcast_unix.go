//go:build unix

package seestar

import "syscall"

// setBroadcast enables SO_BROADCAST on a UDP socket. Without this the
// kernel silently drops datagrams sent to a broadcast address on Linux
// and macOS.
func setBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}
