package main

import (
	"encoding/binary"
	"maps"
	"testing"
)

func TestParseCounters(t *testing.T) {
	got, err := parseCounters("100:0-3, 18DAF110:2-3")
	if err != nil {
		t.Fatal(err)
	}
	want := map[idKey]counterField{
		{id: 0x100}:                      {offset: 0, width: 4},
		{id: 0x18DAF110, extended: true}: {offset: 2, width: 2},
	}
	if !maps.Equal(got, want) {
		t.Errorf("parseCounters = %v, want %v", got, want)
	}

	for _, in := range []string{"100", "100:3", "100:3-1", "100:0-4", "100:-1-2", "100:a-b", "100:0-3,100:4-5"} {
		if _, err := parseCounters(in); err == nil {
			t.Errorf("parseCounters(%q) succeeded, want an error", in)
		}
	}
}

func TestCounterFieldRead(t *testing.T) {
	if v, ok := (counterField{offset: 0, width: 4}).read([]byte{0x00, 0x00, 0x13, 0x88, 9, 9, 9, 9}); !ok || v != 5000 {
		t.Errorf("4-byte read = %d, %v; want 5000", v, ok)
	}
	if v, ok := (counterField{offset: 2, width: 2}).read([]byte{0x02, 0x01, 0x00, 0x07}); !ok || v != 7 {
		t.Errorf("2-byte read at offset 2 = %d, %v; want 7", v, ok)
	}
	if _, ok := (counterField{offset: 0, width: 4}).read([]byte{1, 2, 3}); ok {
		t.Error("read past the end of the payload succeeded")
	}
}

func TestSeqGap(t *testing.T) {
	const m32, m16 = 1 << 32, 1 << 16
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
		{"16-bit rollover", 0xFFFF, 0, m16, 0, false},
		{"16-bit restart", 1000, 0, m16, 0, true},
	}
	for _, c := range cases {
		missed, restart := seqGap(c.last, c.seq, c.modulus)
		if missed != c.missed || restart != c.restart {
			t.Errorf("%s: seqGap(%d, %d) = %d, %v; want %d, %v", c.name, c.last, c.seq, missed, restart, c.missed, c.restart)
		}
	}
}

func TestCountersObserve(t *testing.T) {
	const iface = "test-counters"
	k := idKey{id: 0x100}
	c := newCounters(iface, map[idKey]counterField{k: {offset: 0, width: 4}})

	frame := func(seq uint32) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, seq)
		return b
	}
	// 10, 11, then 12 and 13 lost, then the sender restarts at 0.
	for _, seq := range []uint32{10, 11, 14, 0, 1} {
		c.observe(k, frame(seq))
	}
	c.observe(k, []byte{1, 2}) // too short to hold the counter: ignored

	if got := counterValue(t, framesMissed, iface, "100"); got != 2 {
		t.Errorf("missed = %v, want 2", got)
	}
	if got := counterValue(t, sequenceRestarts, iface, "100"); got != 1 {
		t.Errorf("restarts = %v, want 1", got)
	}
}
