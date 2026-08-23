//go:build !unix

package seestar

import "errors"

// setBroadcast is a stub on non-unix builds. Windows support is not
// wired in this cut — Discover on that build will fail at runtime,
// which is fine as long as we compile cleanly.
func setBroadcast(_ uintptr) error {
	return errors.New("seestar: LAN discovery not supported on this platform yet")
}
