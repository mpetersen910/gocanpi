package main

import (
	"encoding/binary"
	"testing"
)

func TestPadToValidFD(t *testing.T) {
	cases := map[int]int{0: 0, 8: 8, 9: 12, 13: 16, 24: 24, 25: 32, 49: 64, 64: 64, 70: 64}
	for in, want := range cases {
		if got := padToValidFD(in); got != want {
			t.Errorf("padToValidFD(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestParseStandardID(t *testing.T) {
	good := map[string]uint32{"7FF": 0x7FF, "0x7ff": 0x7FF, "100": 0x100, " 250 ": 0x250, "0": 0}
	for in, want := range good {
		got, err := parseStandardID(in)
		if err != nil || got != want {
			t.Errorf("parseStandardID(%q) = %X, %v; want %X", in, got, err, want)
		}
	}
	for _, in := range []string{"800", "18DAF110", "xyz", ""} {
		if _, err := parseStandardID(in); err == nil {
			t.Errorf("parseStandardID(%q) succeeded, want an error", in)
		}
	}
}

func TestCheckFlags(t *testing.T) {
	cases := []struct {
		mode string
		set  []string
		ok   bool
	}{
		{"standard", nil, true},
		{"standard", []string{"iface", "scale", "quiet"}, true},
		{"standard", []string{"burst-every"}, false},
		{"standard", []string{"burst-id"}, false},
		{"burst", []string{"iface", "burst-id", "burst-count", "burst-every"}, true},
		{"burst", []string{"scale"}, false},
		{"pulse", nil, false},
	}
	for _, c := range cases {
		set := map[string]bool{}
		for _, name := range c.set {
			set[name] = true
		}
		if err := checkFlags(c.mode, set); (err == nil) != c.ok {
			t.Errorf("checkFlags(%q, %v) = %v, want ok=%v", c.mode, c.set, err, c.ok)
		}
	}
}

func TestEncode(t *testing.T) {
	ext := message{id: 0x18DAF110, length: 8, extended: true, fill: func([]byte, uint64) {}}
	b := encode(ext, 0)
	if len(b) != canMTU {
		t.Fatalf("classic frame is %d bytes, want %d", len(b), canMTU)
	}
	if got := binary.LittleEndian.Uint32(b[0:4]); got != 0x18DAF110|canEFFFlag {
		t.Errorf("extended can_id = %08X, want the EFF flag set", got)
	}

	fd := message{id: 0x300, length: 10, fd: true, brs: true, fill: func(b []byte, _ uint64) { b[9] = 0xAA }}
	b = encode(fd, 0)
	if len(b) != canFDMTU {
		t.Fatalf("FD frame is %d bytes, want %d", len(b), canFDMTU)
	}
	if b[4] != 12 {
		t.Errorf("FD length = %d, want 10 padded to 12", b[4])
	}
	if b[5] != canFDBRS {
		t.Errorf("FD flags = %02X, want BRS", b[5])
	}
	if b[8+9] != 0xAA {
		t.Errorf("payload byte 9 = %02X, want what fill wrote", b[8+9])
	}
}

// Two senders must never share an ID, so the schedule has to leave the
// burst default alone.
func TestScheduleLeavesBurstIDFree(t *testing.T) {
	for _, m := range schedule {
		if !m.extended && m.id == 0x7FF {
			t.Errorf("schedule message %s uses 7FF, the burst default", m.label)
		}
	}
}
