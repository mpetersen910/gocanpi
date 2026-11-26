package main

import (
	"encoding/binary"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestParseID(t *testing.T) {
	good := map[string]idKey{
		"100":        {id: 0x100},
		"0x7ff":      {id: 0x7FF},
		"18DAF110":   {id: 0x18DAF110, extended: true},
		"00000100":   {id: 0x100, extended: true},
		" 1FFFFFFF ": {id: 0x1FFFFFFF, extended: true},
	}
	for in, want := range good {
		got, err := parseID(in)
		if err != nil || got != want {
			t.Errorf("parseID(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"800", "20000000", "xyz", ""} {
		if _, err := parseID(in); err == nil {
			t.Errorf("parseID(%q) succeeded, want an error", in)
		}
	}
}

func TestRxTime(t *testing.T) {
	want := time.Unix(1764126300, 123456789)
	ts := unix.NsecToTimespec(want.UnixNano())
	data := make([]byte, binary.Size(ts))
	if _, err := binary.Encode(data, binary.NativeEndian, ts); err != nil {
		t.Fatal(err)
	}

	oob := make([]byte, unix.CmsgSpace(len(data)))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	h.Level = unix.SOL_SOCKET
	h.Type = unix.SCM_TIMESTAMPNS
	h.SetLen(unix.CmsgLen(len(data)))
	copy(oob[unix.CmsgLen(0):], data)

	if got := rxTime(oob); !got.Equal(want) {
		t.Errorf("rxTime = %v, want %v", got, want)
	}
	if got := rxTime(nil); time.Since(got) > time.Second {
		t.Errorf("rxTime without a timestamp = %v, want about now", got)
	}
}
