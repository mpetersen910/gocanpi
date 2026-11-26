package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Periodic messages are the backbone of most CAN buses, and their timing is
// visible without understanding a single payload byte. Given each message's
// expected period (from a DBC file, the NMEA 2000 PGN table, or whoever owns
// the bus), the interval between consecutive arrivals says whether a frame
// was on time, late, or never came.

const (
	// An interval this many periods long means the frame was late.
	lateRatio = 1.25
	// From here on the interval is counted as missing frames. A frame
	// delayed by more than half a period looks the same as a lost one;
	// timing alone can't tell them apart.
	missedRatio = 1.5
)

var (
	intervalRatio = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "can_frame_interval_ratio",
		Help:    "Time since the previous frame of the same ID divided by its expected period. 1 is on time, 2 is one frame missing.",
		Buckets: []float64{0.25, 0.5, 0.75, 0.9, 1.1, 1.25, 1.5, 2.5, 3.5, 5, 10},
	}, []string{"iface", "can_id"})

	framesLate = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_late_total",
		Help: "Frames that arrived 1.25 to 1.5 periods after the previous frame of the same ID.",
	}, []string{"iface", "can_id"})

	framesMissedEstimated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_missed_estimated_total",
		Help: "Frames presumed missing because the gap since the previous frame of the same ID reached 1.5 periods or more.",
	}, []string{"iface", "can_id"})
)

// parsePeriods reads a list like "100:5ms,18DAF110:500ms".
func parsePeriods(s string) (map[idKey]time.Duration, error) {
	periods := map[idKey]time.Duration{}
	for _, entry := range splitAndTrim(s) {
		idText, durText, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("%q: want ID:period, e.g. 100:5ms", entry)
		}
		k, err := parseID(idText)
		if err != nil {
			return nil, err
		}
		d, err := time.ParseDuration(strings.TrimSpace(durText))
		if err != nil {
			return nil, fmt.Errorf("%q: %v", entry, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%q: period must be positive", entry)
		}
		if _, dup := periods[k]; dup {
			return nil, fmt.Errorf("%s is listed twice", k.label())
		}
		periods[k] = d
	}
	return periods, nil
}

// classifyInterval turns interval/period into a verdict on the frame that
// just arrived.
func classifyInterval(ratio float64) (late bool, missed int) {
	switch {
	case ratio >= missedRatio:
		return false, int(math.Round(ratio)) - 1
	case ratio >= lateRatio:
		return true, 0
	}
	return false, 0
}

// timing tracks arrival intervals for the messages with a known period on
// one interface. Only that interface's reader goroutine touches it.
type timing struct {
	iface   string
	periods map[idKey]time.Duration
	last    map[idKey]time.Time
}

func newTiming(iface string, periods map[idKey]time.Duration) *timing {
	// Create every series up front so dashboards read 0 rather than
	// "no data" before the first late or missing frame.
	for k := range periods {
		id := k.label()
		intervalRatio.WithLabelValues(iface, id)
		framesLate.WithLabelValues(iface, id)
		framesMissedEstimated.WithLabelValues(iface, id)
	}
	return &timing{iface: iface, periods: periods, last: map[idKey]time.Time{}}
}

func (t *timing) observe(k idKey, at time.Time) {
	period, ok := t.periods[k]
	if !ok {
		return
	}
	prev, seen := t.last[k]
	t.last[k] = at
	if !seen {
		return
	}

	id := k.label()
	ratio := float64(at.Sub(prev)) / float64(period)
	intervalRatio.WithLabelValues(t.iface, id).Observe(ratio)

	late, missed := classifyInterval(ratio)
	if late {
		framesLate.WithLabelValues(t.iface, id).Inc()
	}
	if missed > 0 {
		framesMissedEstimated.WithLabelValues(t.iface, id).Add(float64(missed))
	}
}
