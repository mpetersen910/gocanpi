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
// be counted exactly instead of inferred from timing.

var (
	framesMissed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_missed_total",
		Help: "Frames missing according to the message's own rolling counter.",
	}, []string{"iface", "can_id"})

	sequenceRestarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_sequence_restarts_total",
		Help: "Times a message's rolling counter went backwards, meaning its sender restarted.",
	}, []string{"iface", "can_id"})
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

// parseCounters reads a list like "100:0-3": bytes 0 through 3 of ID 100.
func parseCounters(s string) (map[idKey]counterField, error) {
	fields := map[idKey]counterField{}
	for _, entry := range splitAndTrim(s) {
		idText, rangeText, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("%q: want ID:first-last, e.g. 100:0-3", entry)
		}
		k, err := parseID(idText)
		if err != nil {
			return nil, err
		}
		firstText, lastText, ok := strings.Cut(strings.TrimSpace(rangeText), "-")
		if !ok {
			return nil, fmt.Errorf("%q: want a byte range like 0-3", entry)
		}
		first, err1 := strconv.Atoi(firstText)
		last, err2 := strconv.Atoi(lastText)
		if err1 != nil || err2 != nil || first < 0 || last < first {
			return nil, fmt.Errorf("%q: want a byte range like 0-3", entry)
		}
		if width := last - first + 1; width > 4 {
			return nil, fmt.Errorf("%q: counters can be at most 4 bytes", entry)
		}
		if _, dup := fields[k]; dup {
			return nil, fmt.Errorf("%s is listed twice", k.label())
		}
		fields[k] = counterField{offset: first, width: last - first + 1}
	}
	return fields, nil
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

// counters tracks the rolling counters configured for one interface. Only
// that interface's reader goroutine touches it.
type counters struct {
	iface  string
	fields map[idKey]counterField
	last   map[idKey]uint32
}

func newCounters(iface string, fields map[idKey]counterField) *counters {
	for k := range fields {
		id := k.label()
		framesMissed.WithLabelValues(iface, id)
		sequenceRestarts.WithLabelValues(iface, id)
	}
	return &counters{iface: iface, fields: fields, last: map[idKey]uint32{}}
}

func (c *counters) observe(k idKey, data []byte) {
	field, ok := c.fields[k]
	if !ok {
		return
	}
	seq, ok := field.read(data)
	if !ok {
		return
	}
	prev, seen := c.last[k]
	c.last[k] = seq
	if !seen {
		return
	}

	missed, restart := seqGap(prev, seq, field.modulus())
	if restart {
		sequenceRestarts.WithLabelValues(c.iface, k.label()).Inc()
		log.Printf("%s: %s counter went from %d back to %d; sender restarted", c.iface, k.label(), prev, seq)
		return
	}
	if missed > 0 {
		framesMissed.WithLabelValues(c.iface, k.label()).Add(float64(missed))
	}
}
