package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	canEFFFlag = 0x80000000
	canSFFMask = 0x7FF
	canMTU     = 16
	canFDMTU   = 72
	canFDBRS   = 0x01

	// How long sendRetry waits before offering a frame again after ENOBUFS.
	// Roughly one classic frame time at 500 kbit/s, so a slot has usually
	// opened up by the next attempt.
	retryBackoff = 200 * time.Microsecond
)

// validFDLengths — CAN-FD does NOT allow arbitrary payload sizes. Above 8
// bytes only these are legal, because the DLC field encodes a size class
// rather than a count. Writing a frame with len=10 fails at the socket with
// a bare EINVAL and no hint as to why.
var validFDLengths = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64}

func padToValidFD(n int) int {
	for _, v := range validFDLengths {
		if v >= n {
			return v
		}
	}
	return 64
}

// message is one entry in a transmit schedule.
type message struct {
	id       uint32
	period   time.Duration
	length   int
	fd       bool
	brs      bool
	extended bool
	label    string
	// fill writes the payload. seq is the per-message frame counter.
	fill func(buf []byte, seq uint64)
}

// parseStandardID reads an 11-bit CAN ID written in hex, with or without a
// 0x prefix, the way candump prints it.
func parseStandardID(s string) (uint32, error) {
	digits := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	v, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not a hex CAN ID", s)
	}
	if v > canSFFMask {
		return 0, fmt.Errorf("%q is above 7FF, the largest standard CAN ID", s)
	}
	return uint32(v), nil
}

// openCAN opens a raw SocketCAN socket for transmission.
func openCAN(iface string) (int, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW, unix.CAN_RAW)
	if err != nil {
		return -1, err
	}
	// Needed to WRITE FD frames as well as read them — without it the
	// kernel rejects any write longer than 16 bytes.
	if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// encode builds the on-wire struct can_frame / canfd_frame.
func encode(m message, seq uint64) []byte {
	size := canMTU
	length := m.length
	if m.fd {
		size = canFDMTU
		length = padToValidFD(length)
	}

	buf := make([]byte, size)

	id := m.id
	if m.extended {
		id |= canEFFFlag
	}
	binary.LittleEndian.PutUint32(buf[0:4], id)
	buf[4] = byte(length)
	if m.fd && m.brs {
		buf[5] = canFDBRS
	}

	m.fill(buf[8:8+m.length], seq)
	return buf
}

// transmitter owns the socket. Several goroutines share it, so writes are
// serialised, and it keeps the counts the summary log prints.
type transmitter struct {
	fd      int
	mu      sync.Mutex
	sent    uint64
	failed  uint64
	retries uint64
}

// send makes a single attempt. A frame the kernel won't take is counted and
// dropped, which is what a periodic sender does with data that would be
// stale by the time it got through.
func (t *transmitter) send(b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := unix.Write(t.fd, b); err != nil {
		t.fail(err)
		return
	}
	t.sent++
}

// sendRetry keeps offering the frame while the tx queue is full, so a burst
// really reaches the bus instead of mostly bouncing off ENOBUFS. It gives up
// when stop closes or the write fails for any other reason.
func (t *transmitter) sendRetry(b []byte, stop <-chan struct{}) {
	for {
		t.mu.Lock()
		_, err := unix.Write(t.fd, b)
		switch {
		case err == nil:
			t.sent++
		case errors.Is(err, unix.ENOBUFS):
			t.retries++
		default:
			t.fail(err)
		}
		t.mu.Unlock()

		if !errors.Is(err, unix.ENOBUFS) {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		time.Sleep(retryBackoff)
	}
}

// fail records a dropped frame. Callers hold t.mu.
func (t *transmitter) fail(err error) {
	t.failed++
	// ENOBUFS means the interface tx queue is full — the bus can't
	// carry what we're asking for. Real backpressure, worth seeing.
	if t.failed%100 == 1 {
		log.Printf("write: %v", err)
	}
}

func (t *transmitter) counts() (sent, failed, retries uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sent, t.failed, t.retries
}
