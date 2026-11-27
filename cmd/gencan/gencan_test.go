package main

import (
	"encoding/binary"
	"testing"
	"time"
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
		{"standard", []string{"event-heartbeat"}, false},
		{"burst", []string{"iface", "burst-id", "burst-count", "burst-every"}, true},
		{"burst", []string{"scale"}, false},
		{"burst", []string{"event-id"}, false},
		{"event", []string{"iface", "quiet", "event-id", "event-deadband", "event-min-gap", "event-heartbeat", "event-seed"}, true},
		{"event", []string{"burst-count"}, false},
		{"event", []string{"scale"}, false},
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

// Two senders must never share an ID, so the schedule has to leave the burst
// and event defaults alone.
func TestDefaultIDsAreUnique(t *testing.T) {
	if defaultBurstID == defaultEventID {
		t.Fatal("burst and event mode default to the same ID")
	}
	for _, m := range schedule {
		if !m.extended && (m.id == defaultBurstID || m.id == defaultEventID) {
			t.Errorf("schedule message %s uses %03X, a burst or event default", m.label, m.id)
		}
	}
}

func TestShouldSend(t *testing.T) {
	cfg := eventConfig{deadband: 0.5, minGap: 100 * time.Millisecond, heartbeat: 5 * time.Second}
	noHeartbeat := cfg
	noHeartbeat.heartbeat = 0

	t0 := time.Unix(1764126300, 0)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	sent := sensorState{sentAny: true, lastSent: t0, lastValue: 21.0}

	cases := []struct {
		name  string
		cfg   eventConfig
		state sensorState
		now   time.Time
		value float64
		want  sendReason
	}{
		{"first reading", cfg, sensorState{}, t0, 21.0, sendFirst},
		{"change below the deadband", cfg, sent, at(time.Second), 21.4, noSend},
		{"change up", cfg, sent, at(time.Second), 21.5, sendChange},
		{"change down", cfg, sent, at(time.Second), 20.4, sendChange},
		{"change held by the min gap", cfg, sent, at(40 * time.Millisecond), 22.0, noSend},
		{"change once the min gap passes", cfg, sent, at(100 * time.Millisecond), 22.0, sendChange},
		{"quiet before the heartbeat", cfg, sent, at(4900 * time.Millisecond), 21.1, noSend},
		{"heartbeat", cfg, sent, at(5 * time.Second), 21.1, sendHeartbeat},
		{"change wins over heartbeat", cfg, sent, at(6 * time.Second), 22.0, sendChange},
		{"heartbeat disabled", noHeartbeat, sent, at(time.Minute), 21.1, noSend},
	}
	for _, c := range cases {
		if got := shouldSend(c.state, c.now, c.value, c.cfg); got != c.want {
			t.Errorf("%s: shouldSend = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEncodeTemp(t *testing.T) {
	b := make([]byte, 8)
	encodeTemp(b, 21.34, 7)
	if got := int16(binary.BigEndian.Uint16(b[0:2])); got != 213 {
		t.Errorf("21.34 C encoded as %d, want 213 tenths", got)
	}
	if b[2] != 7 {
		t.Errorf("counter byte = %d, want 7", b[2])
	}

	b = make([]byte, 8)
	encodeTemp(b, -5.26, 255)
	if got := int16(binary.BigEndian.Uint16(b[0:2])); got != -53 {
		t.Errorf("-5.26 C encoded as %d, want -53 tenths", got)
	}
	if b[2] != 255 {
		t.Errorf("counter byte = %d, want 255", b[2])
	}
	for i, v := range b[3:] {
		if v != 0 {
			t.Errorf("byte %d = %02X, want 0", i+3, v)
		}
	}
}
