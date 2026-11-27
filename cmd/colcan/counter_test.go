package main

import (
	"encoding/binary"
	"testing"
)

func TestParseByteRange(t *testing.T) {
	good := map[string]counterField{
		"0-3":   {offset: 0, width: 4},
		"2-2":   {offset: 2, width: 1},
		" 4-5 ": {offset: 4, width: 2},
	}
	for in, want := range good {
		got, err := parseByteRange(in)
		if err != nil || got != want {
			t.Errorf("parseByteRange(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "3", "3-1", "0-4", "-1-2", "a-b"} {
		if _, err := parseByteRange(in); err == nil {
			t.Errorf("parseByteRange(%q) succeeded, want an error", in)
		}
	}
}

func TestCounterFieldRead(t *testing.T) {
	if v, ok := (counterField{offset: 0, width: 4}).read([]byte{0x00, 0x00, 0x13, 0x88, 9, 9, 9, 9}); !ok || v != 5000 {
		t.Errorf("4-byte read = %d, %v; want 5000", v, ok)
	}
	if v, ok := (counterField{offset: 2, width: 1}).read([]byte{0x00, 0xD5, 0x07, 0}); !ok || v != 7 {
		t.Errorf("1-byte read at offset 2 = %d, %v; want 7", v, ok)
	}
	if _, ok := (counterField{offset: 0, width: 4}).read([]byte{1, 2, 3}); ok {
		t.Error("read past the end of the payload succeeded")
	}
}

func TestSeqGap(t *testing.T) {
	const m32, m8 = 1 << 32, 1 << 8
	cases := []struct {
		name      string
		last, seq uint32
		modulus   uint64
		missed    uint64
		restart   bool
	}{
		{"next", 5000, 5001, m32, 0, false},
		{"duplicate", 5000, 5000, m32, 0, false},
		{"gap", 5000, 5004, m32, 3, false},
		{"rollover", 0xFFFFFFFF, 0, m32, 0, false},
		{"gap across rollover", 0xFFFFFFFE, 1, m32, 2, false},
		{"restart", 5000, 0, m32, 0, true},
		{"largest gap", 0, 1<<31 - 1, m32, 1<<31 - 2, false},
		{"half the range is a restart", 0, 1 << 31, m32, 0, true},
		{"8-bit rollover", 255, 0, m8, 0, false},
		{"8-bit gap", 250, 3, m8, 8, false},
		{"8-bit restart", 100, 0, m8, 0, true},
	}
	for _, c := range cases {
		missed, restart := seqGap(c.last, c.seq, c.modulus)
		if missed != c.missed || restart != c.restart {
			t.Errorf("%s: seqGap(%d, %d) = %d, %v; want %d, %v", c.name, c.last, c.seq, missed, restart, c.missed, c.restart)
		}
	}
}

func TestCounterMonitor(t *testing.T) {
	const iface = "test-counter"
	field := counterField{offset: 0, width: 4}
	s := messageSpec{key: idKey{id: 0x100}, name: "fast_counter", kind: typePeriodic, counter: &field}
	m := newCounterMonitor(iface, s)

	frame := func(seq uint32) Frame {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, seq)
		return Frame{Data: b}
	}
	// 10, 11, then 12 and 13 lost, then the sender restarts at 0.
	for _, seq := range []uint32{10, 11, 14, 0, 1} {
		m.observe(frame(seq))
	}
	m.observe(Frame{Data: []byte{1, 2}}) // too short to hold the counter: ignored

	labels := s.labels(iface)
	if got := counterValue(t, framesMissed, labels...); got != 2 {
		t.Errorf("missed = %v, want 2", got)
	}
	if got := counterValue(t, sequenceRestarts, labels...); got != 1 {
		t.Errorf("restarts = %v, want 1", got)
	}
}
