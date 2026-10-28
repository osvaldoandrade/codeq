package raft

import (
	"fmt"
	"net"
	"strconv"
)

// advertisedAddr is the address a voter announces to its peers in every Raft
// RPC header. It is kept verbatim (a DNS name stays a DNS name) because peers
// never dial it: they dial the address in the replicated configuration.
type advertisedAddr string

// Network reports "tcp", the only network the Raft transports use.
func (a advertisedAddr) Network() string { return "tcp" }

// String returns the advertised address verbatim.
func (a advertisedAddr) String() string { return string(a) }

// ValidateAdvertiseAddr reports whether addr can identify exactly one voter.
//
// hashicorp/raft records a vote as the candidate's advertised address bytes
// and grants a second request in the same term when those bytes match the
// recorded ones ("duplicate requestVote"). Voters that all advertise the same
// wildcard listener address ("[::]:7000") therefore look like one candidate:
// a voter that already voted grants a competing candidate too, and one term
// can elect two leaders. A wildcard host, a missing host or a non-dialable
// port is rejected.
func ValidateAdvertiseAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("raft advertise address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("raft advertise address %q has no host", addr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("raft advertise address %q is a wildcard address", addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("raft advertise address %q has an invalid port", addr)
	}
	return nil
}
