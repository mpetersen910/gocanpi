package main

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestClassifyInterval(t *testing.T) {
	cases := []struct {
		ratio  float64
		late   bool
		missed int
	}{
		{0.5, false, 0},
		{1.0, false, 0},
		{1.24, false, 0},
		{1.25, true, 0},
		{1.49, true, 0},
		{1.5, false, 1},
		{2.0, false, 1},
		{2.49, false, 1},
		{2.5, false, 2},
		{3.0, false, 2},
	}
	for _, c := range cases {
		late, missed := classifyInterval(c.ratio)
		if late != c.late || missed != c.missed {
			t.Errorf("classifyInterval(%v) = %v, %d; want %v, %d", c.ratio, late, missed, c.late, c.missed)
		}
	}
}

func TestPeriodicMonitor(t *testing.T) {
	const iface = "test-periodic"
	s := messageSpec{key: idKey{id: 0x100}, name: "fast_counter", kind: typePeriodic, period: 5 * time.Millisecond}
	m := newPeriodicMonitor(iface, s)

	for _, ms := range []int{0, 5, 10, 17, 22, 32, 37} {
		m.observe(frameAt(ms))
	}
	// Intervals: 5, 5, 7 (late), 5, 10 (one missing), 5.

	labels := s.labels(iface)
	if got := counterValue(t, framesLate, labels...); got != 1 {
		t.Errorf("late = %v, want 1", got)
	}
	if got := counterValue(t, framesMissedEstimated, labels...); got != 1 {
		t.Errorf("estimated missed = %v, want 1", got)
	}
	if got := histogramCount(t, intervalRatio, labels...); got != 6 {
		t.Errorf("interval samples = %d, want 6", got)
	}
}

var testEpoch = time.Unix(1764126300, 0)

// frameAt is a frame that arrived ms milliseconds after testEpoch.
func frameAt(ms int, data ...byte) Frame {
	return Frame{RxTime: testEpoch.Add(time.Duration(ms) * time.Millisecond), Data: data}
}

func counterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, g *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func histogramCount(t *testing.T, h *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.WithLabelValues(labels...).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}
