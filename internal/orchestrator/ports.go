package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"net"

	"ts3panel/internal/model"
)

// Port layout from the specification (§10.1). Every instance owns four
// consecutive-ish ports derived from one index; query ports stay on loopback.
const (
	VoicePortBase        = 9987
	FileTransferBase     = 30033
	QueryPortBase        = 10011
	QuerySSHPortBase     = 10022
	maxPortAttempts      = 50
	unprivilegedPortBase = 1024
)

// ErrPortExhausted is returned when 50 consecutive indexes have no free group.
var ErrPortExhausted = errors.New("port range exhausted")

// Probe tries to take a port and returns a closer so the caller can release it
// immediately. Tests inject a fake probe; production uses bindProbe.
type Probe func(network, address string) (io.Closer, error)

// PortsFor maps an index to the four ports of one instance.
func PortsFor(index int) model.Ports {
	return model.Ports{
		Voice:        VoicePortBase + index,
		FileTransfer: FileTransferBase + index,
		Query:        QueryPortBase + index,
		QuerySSH:     QuerySSHPortBase + index,
	}
}

// bindProbe listens on all interfaces. The listener is closed by the caller;
// binding proves the port is free at this moment, not permanently.
func bindProbe(network, address string) (io.Closer, error) {
	switch network {
	case "tcp":
		return net.Listen("tcp", address)
	case "udp":
		conn, err := net.ListenPacket("udp", address)
		if err != nil {
			return nil, err
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("unsupported network %q", network)
	}
}

// Allocate finds the first index at or after start whose four ports are free
// for both TCP and UDP, and returns the ports plus that index. It probes at
// most maxPortAttempts indexes and returns ErrPortExhausted otherwise.
//
// The caller must persist next_index = index+1 so deleted instances never hand
// their ports to a new instance (stale DNS caches / reconnecting clients).
func Allocate(start int, probe Probe) (model.Ports, int, error) {
	if start < 0 {
		start = 0
	}
	if probe == nil {
		probe = bindProbe
	}
	for index := start; index < start+maxPortAttempts; index++ {
		ports := PortsFor(index)
		if ports.Query > 65535 || ports.FileTransfer > 65535 {
			return model.Ports{}, -1, ErrPortExhausted
		}
		if portsFree(ports, probe) {
			return ports, index, nil
		}
	}
	return model.Ports{}, -1, ErrPortExhausted
}

// portsFree checks all four ports on both protocols. A group is free only when
// every bind succeeds; already-acquired listeners are closed before moving on.
func portsFree(ports model.Ports, probe Probe) bool {
	targets := []int{ports.Voice, ports.FileTransfer, ports.Query, ports.QuerySSH}
	acquired := make([]io.Closer, 0, len(targets)*2)
	defer func() {
		for _, c := range acquired {
			_ = c.Close()
		}
	}()

	for _, port := range targets {
		if port < unprivilegedPortBase || port > 65535 {
			return false
		}
		address := fmt.Sprintf(":%d", port)
		for _, network := range []string{"tcp", "udp"} {
			c, err := probe(network, address)
			if err != nil {
				return false
			}
			if c != nil {
				acquired = append(acquired, c)
			}
		}
	}
	return true
}
