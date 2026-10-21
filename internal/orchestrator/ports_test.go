package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

type fakeCloser struct{ closed *int }

func (c fakeCloser) Close() error {
	*c.closed++
	return nil
}

type fakeProbe struct {
	busy   map[string]bool
	probed []string
	closed int
}

func newFakeProbe(busy ...string) *fakeProbe {
	f := &fakeProbe{busy: make(map[string]bool, len(busy))}
	for _, key := range busy {
		f.busy[key] = true
	}
	return f
}

func (f *fakeProbe) probe(network, address string) (io.Closer, error) {
	key := network + ":" + address
	f.probed = append(f.probed, key)
	if f.busy[key] {
		return nil, errors.New("address already in use")
	}
	return fakeCloser{closed: &f.closed}, nil
}

func TestPortsFor(t *testing.T) {
	p := PortsFor(0)
	if p.Voice != 9987 || p.FileTransfer != 30033 || p.Query != 10011 || p.QuerySSH != 10022 {
		t.Fatalf("index 0 = %+v", p)
	}
	p = PortsFor(7)
	if p.Voice != 9994 || p.FileTransfer != 30040 || p.Query != 10018 || p.QuerySSH != 10029 {
		t.Fatalf("index 7 = %+v", p)
	}
}

func TestAllocateSkipsOccupiedVoicePort(t *testing.T) {
	probe := newFakeProbe("udp::9987")
	ports, index, err := Allocate(0, probe.probe)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if index != 1 {
		t.Fatalf("index = %d, want 1", index)
	}
	want := PortsFor(1)
	if ports != want {
		t.Fatalf("ports = %+v, want %+v", ports, want)
	}
	if probe.closed != 9 {
		t.Fatalf("closed listeners = %d, want 9 (index 0 closed voice TCP, index 1 closed all eight)", probe.closed)
	}
}

func TestAllocateRequiresBothProtocols(t *testing.T) {
	// Only UDP of the file-transfer port of index 1 is busy. A TCP-only check
	// would wrongly accept index 1.
	probe := newFakeProbe("udp::30034")
	ports, index, err := Allocate(1, probe.probe)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if index != 2 || ports.Voice != 9989 {
		t.Fatalf("index=%d ports=%+v, want index 2", index, ports)
	}
	// Index 1 acquired three listeners before the busy UDP port and closed
	// them; index 2 acquires all 8 and closes them. Nothing is leaked.
	if probe.closed != 11 {
		t.Fatalf("closed listeners = %d, want 11", probe.closed)
	}
}

func TestAllocateExhausted(t *testing.T) {
	probe := &fakeProbe{busy: map[string]bool{}}
	for i := 0; i < maxPortAttempts; i++ {
		probe.busy[fmt.Sprintf("tcp::%d", VoicePortBase+i)] = true
	}
	_, _, err := Allocate(0, probe.probe)
	if !errors.Is(err, ErrPortExhausted) {
		t.Fatalf("err = %v, want ErrPortExhausted", err)
	}
	if len(probe.probed) != maxPortAttempts {
		t.Fatalf("probed %d addresses, want %d", len(probe.probed), maxPortAttempts)
	}
	if probe.closed != 0 {
		t.Fatalf("closed = %d, want 0", probe.closed)
	}
}

func TestAllocateHonorsStartIndex(t *testing.T) {
	probe := newFakeProbe()
	ports, index, err := Allocate(5, probe.probe)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if index != 5 || ports.Voice != 9992 {
		t.Fatalf("index=%d voice=%d", index, ports.Voice)
	}
}
