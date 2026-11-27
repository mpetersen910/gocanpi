package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Many messages carry a rolling counter (AUTOSAR E2E protection puts one in
// every safety-relevant frame). When the spec says where it lives, drops can
// be counted exactly instead of inferred from timing, and for an event
// message, whose timing promises nothing, it is the only way to see them.

var (
	framesMissed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_missed_total",
		Help: "Frames missing according to the message's own rolling counter.",
	}, catalogLabels)

	sequenceRestarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_sequence_restarts_total",
		Help: "Times a message's rolling counter went backwards, meaning its sender restarted.",
	}, catalogLabels)
)

// counterField is where a message keeps its counter: width bytes starting at
// offset, big-endian.
type counterField struct {
	offset int
	width  int
}

func (c counterField) read(data []byte) (uint32, bool) {
	end := c.offset + c.width
	if len(data) < end {
		return 0, false
	}
	var v uint32
	for _, b := range data[c.offset:end] {
		v = v<<8 | uint32(b)
	}
	return v, true
}

// modulus is where the counter wraps back to 0.
func (c counterField) modulus() uint64 {
	return 1 << (8 * c.width)
}

// parseByteRange reads "first-last", e.g. "0-3" for bytes 0 through 3.
func parseByteRange(s string) (counterField, error) {
	firstText, lastText, ok := strings.Cut(strings.TrimSpace(s), "-")
	first, err1 := strconv.Atoi(firstText)
	last, err2 := strconv.Atoi(lastText)
	if !ok || err1 != nil || err2 != nil || first < 0 || last < first {
		return counterField{}, fmt.Errorf("%q: want a byte range like 0-3", s)
	}
	if width := last - first + 1; width > 4 {
		return counterField{}, fmt.Errorf("%q: a counter can be at most 4 bytes", s)
	}
	return counterField{offset: first, width: last - first + 1}, nil
}

// seqGap compares a counter with its previous value. The difference is taken
// modulo the counter's range, so rolling over to 0 is a normal step. A
// difference in the upper half of the range can only be a step backwards: the
// sender restarted and began counting from 0 again.
func seqGap(last, seq uint32, modulus uint64) (missed uint64, restart bool) {
	d := (uint64(seq) - uint64(last)) % modulus
	switch {
	case d <= 1: // next in sequence, or a duplicate
		return 0, false
	case d < modulus/2:
		return d - 1, false
	}
	return 0, true
}

type counterMonitor struct {
	iface string
	spec  messageSpec
	last  uint32
	seen  bool

	missed   prometheus.Counter
	restarts prometheus.Counter
}

func newCounterMonitor(iface string, s messageSpec) *counterMonitor {
	l := s.labels(iface)
	return &counterMonitor{
		iface:    iface,
		spec:     s,
		missed:   framesMissed.WithLabelValues(l...),
		restarts: sequenceRestarts.WithLabelValues(l...),
	}
}

func (m *counterMonitor) observe(f Frame) {
	seq, ok := m.spec.counter.read(f.Data)
	if !ok {
		return
	}
	prev, seen := m.last, m.seen
	m.last, m.seen = seq, true
	if !seen {
		return
	}

	missed, restart := seqGap(prev, seq, m.spec.counter.modulus())
	if restart {
		m.restarts.Inc()
		log.Printf("%s: %s counter went from %d back to %d; sender restarted", m.iface, m.spec.name, prev, seq)
		return
	}
	if missed > 0 {
		m.missed.Add(float64(missed))
	}
}
