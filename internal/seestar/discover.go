// Package seestar talks to a ZWO Seestar smart telescope over its
// undocumented local-network protocols — UDP discovery on 4720, a
// line-framed JSON-RPC command socket on 4700, and the built-in
// HTTP file server on 80.
//
// Reverse-engineered from ZWO's own Android app; mechanics documented
// in third-party client `seestarpy` (astronomyk/seestarpy on GitHub).
// Nothing in this package sends anything to the internet — it only
// probes the local subnet the CLI is running on.
package seestar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// DiscoveryPort is the UDP port every Seestar listens on for the
// firmware's `scan_iscope` probe. The ZWO phone app uses the same one.
const DiscoveryPort = 4720

// Device is one scope discovered on the LAN.
type Device struct {
	// IP is the scope's current IPv4 address on the discovering host's
	// subnet. Can change between sessions (DHCP lease renewals, Wi-Fi
	// vs wired) — never cache across runs, always re-discover.
	IP string
	// Serial is the scope's serial number (e.g. `a3497936`). Stable
	// across reboots and firmware upgrades — the right key for a
	// per-scope state file.
	Serial string
	// Model is the vendor-reported model string (e.g. `Seestar S30`,
	// `Seestar S50`, `Seestar S30 Pro`). Whitespace preserved verbatim.
	Model string
}

// scanReply is the on-the-wire shape of the answer datagram. Fields
// beyond `sn`/`product_model` are ignored — they're used by the phone
// app but not by us.
type scanReply struct {
	Result struct {
		SN           string `json:"sn"`
		ProductModel string `json:"product_model"`
	} `json:"result"`
}

// Discover broadcasts a `scan_iscope` probe on UDP 4720 and returns
// every scope that answers within timeout. Sends the probe every
// ~500ms so a single dropped datagram doesn't lose a scope. Blocks
// for the full timeout — most replies arrive within ~150ms so the
// tail is just insurance.
//
// Requires no privileges beyond opening a UDP socket. Fails if the
// host has no route to a broadcast address (rare — VPN-only setups).
func Discover(ctx context.Context, timeout time.Duration) ([]Device, error) {
	targets, err := broadcastTargets()
	if err != nil {
		return nil, fmt.Errorf("enumerate broadcast targets: %w", err)
	}
	if len(targets) == 0 {
		return nil, errors.New("no broadcast targets available — is the network up?")
	}

	sock, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open udp socket: %w", err)
	}
	defer sock.Close()

	// Enable SO_BROADCAST. Without this the sends silently drop on Linux.
	rc, err := sock.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("get raw conn: %w", err)
	}
	var setErr error
	_ = rc.Control(func(fd uintptr) {
		setErr = setBroadcast(fd)
	})
	if setErr != nil {
		return nil, fmt.Errorf("set SO_BROADCAST: %w", setErr)
	}

	probe := []byte(`{"id":1,"method":"scan_iscope","params":""}` + "\r\n")

	// Sender goroutine: retransmit every 500ms until context cancelled.
	sendCtx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			for _, tgt := range targets {
				_, _ = sock.WriteTo(probe, tgt)
			}
			select {
			case <-sendCtx.Done():
				return
			case <-t.C:
			}
		}
	}()

	deadline := time.Now().Add(timeout)
	_ = sock.SetReadDeadline(deadline)

	found := make(map[string]Device)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		n, from, err := sock.ReadFromUDP(buf)
		if err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				break
			}
			continue
		}
		var reply scanReply
		if json.Unmarshal(buf[:n], &reply) != nil {
			continue
		}
		if reply.Result.SN == "" {
			continue
		}
		found[reply.Result.SN] = Device{
			IP:     from.IP.String(),
			Serial: reply.Result.SN,
			Model:  strings.TrimSpace(reply.Result.ProductModel),
		}
	}

	out := make([]Device, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	return out, nil
}

// broadcastTargets returns every UDPAddr worth sending a probe to on
// this host. Uses per-interface directed broadcasts (e.g. `192.168.1.255`)
// first; on a multi-homed host the limited broadcast `255.255.255.255`
// frequently leaves through the wrong interface and reaches nothing.
// The limited broadcast is appended as a fallback so a plain single-NIC
// laptop still works.
func broadcastTargets() ([]*net.UDPAddr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	out := make([]*net.UDPAddr, 0, 4)
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagBroadcast == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			// Compute the directed broadcast for this subnet.
			mask := ipnet.Mask
			if len(mask) != 4 {
				continue
			}
			bc := net.IPv4(
				ip4[0]|^mask[0],
				ip4[1]|^mask[1],
				ip4[2]|^mask[2],
				ip4[3]|^mask[3],
			)
			key := bc.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, &net.UDPAddr{IP: bc, Port: DiscoveryPort})
		}
	}
	// Fallback: limited broadcast.
	if _, ok := seen["255.255.255.255"]; !ok {
		out = append(out, &net.UDPAddr{IP: net.IPv4(255, 255, 255, 255), Port: DiscoveryPort})
	}
	return out, nil
}
