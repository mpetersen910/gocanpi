package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Event messages are sent when something changes, so there is no period to
// be late against. What a spec can still promise is a minimum gap (the
// sender's inhibit time, so it can't flood the bus) and a maximum silence
// (its heartbeat, so quiet still means alive). Those are the checks.

var (
	intervalSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "can_frame_interval_seconds",
		Help:    "Time since the previous frame of an event message.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 7.5, 10, 30, 60},
	}, catalogLabels)

	framesTooSoon = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_too_soon_total",
		Help: "Frames of an event message that arrived sooner after the previous one than its min_gap allows.",
	}, catalogLabels)

	framesOverdue = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_overdue_total",
		Help: "Frames of an event message that arrived after more than its max_silence.",
	}, catalogLabels)
)

type eventMonitor struct {
	minGap     time.Duration
	maxSilence time.Duration
	last       time.Time

	interval prometheus.Observer
	tooSoon  prometheus.Counter
	overdue  prometheus.Counter
}

func newEventMonitor(iface string, s messageSpec) *eventMonitor {
	l := s.labels(iface)
	return &eventMonitor{
		minGap:     s.minGap,
		maxSilence: s.maxSilence,
		interval:   intervalSeconds.WithLabelValues(l...),
		tooSoon:    framesTooSoon.WithLabelValues(l...),
		overdue:    framesOverdue.WithLabelValues(l...),
	}
}

func (m *eventMonitor) observe(f Frame) {
	prev := m.last
	m.last = f.RxTime
	if prev.IsZero() {
		return
	}

	gap := f.RxTime.Sub(prev)
	m.interval.Observe(gap.Seconds())
	if m.minGap > 0 && gap < m.minGap {
		m.tooSoon.Inc()
	}
	if gap > m.maxSilence {
		m.overdue.Inc()
	}
}
