package main

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Periodic messages are the backbone of most CAN buses, and their timing is
// visible without understanding a single payload byte. Given each message's
// expected period, the interval between consecutive arrivals says whether a
// frame was on time, late, or never came.

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
		Help:    "Time since the previous frame of a periodic message divided by its period. 1 is on time, 2 is one frame missing.",
		Buckets: []float64{0.25, 0.5, 0.75, 0.9, 1.1, 1.25, 1.5, 2.5, 3.5, 5, 10},
	}, catalogLabels)

	framesLate = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_late_total",
		Help: "Frames of a periodic message that arrived 1.25 to 1.5 periods after the previous one.",
	}, catalogLabels)

	framesMissedEstimated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_missed_estimated_total",
		Help: "Frames of a periodic message presumed missing because the gap since the previous one reached 1.5 periods or more.",
	}, catalogLabels)
)

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

type periodicMonitor struct {
	period time.Duration
	last   time.Time

	ratio  prometheus.Observer
	late   prometheus.Counter
	missed prometheus.Counter
}

func newPeriodicMonitor(iface string, s messageSpec) *periodicMonitor {
	l := s.labels(iface)
	return &periodicMonitor{
		period: s.period,
		ratio:  intervalRatio.WithLabelValues(l...),
		late:   framesLate.WithLabelValues(l...),
		missed: framesMissedEstimated.WithLabelValues(l...),
	}
}

func (m *periodicMonitor) observe(f Frame) {
	prev := m.last
	m.last = f.RxTime
	if prev.IsZero() {
		return
	}

	ratio := float64(f.RxTime.Sub(prev)) / float64(m.period)
	m.ratio.Observe(ratio)

	late, missed := classifyInterval(ratio)
	if late {
		m.late.Inc()
	}
	if missed > 0 {
		m.missed.Add(float64(missed))
	}
}
