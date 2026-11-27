package main

import (
	"maps"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestParsePeriods(t *testing.T) {
	got, err := parsePeriods("100:5ms, 18DAF110:500ms,0x300:25ms")
	if err != nil {
		t.Fatal(err)
	}
	want := map[idKey]time.Duration{
		{id: 0x100}:                      5 * time.Millisecond,
		{id: 0x18DAF110, extended: true}: 500 * time.Millisecond,
		{id: 0x300}:                      25 * time.Millisecond,
	}
	if !maps.Equal(got, want) {
		t.Errorf("parsePeriods = %v, want %v", got, want)
	}

	if got, err := parsePeriods(""); err != nil || len(got) != 0 {
		t.Errorf("parsePeriods(\"\") = %v, %v; want empty", got, err)
	}

	for _, in := range []string{"100", "100:fast", "100:0s", "100:-5ms", "800:5ms", "zz:5ms", "100:5ms,100:10ms"} {
		if _, err := parsePeriods(in); err == nil {
			t.Errorf("parsePeriods(%q) succeeded, want an error", in)
		}
	}
}

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

func TestTimingObserve(t *testing.T) {
	const iface = "test-timing"
	k := idKey{id: 0x100}
	tr := newTiming(iface, map[idKey]time.Duration{k: 5 * time.Millisecond})

	start := time.Unix(1764126300, 0)
	for _, ms := range []int{0, 5, 10, 17, 22, 32, 37} {
		tr.observe(k, start.Add(time.Duration(ms)*time.Millisecond))
	}
	// Intervals: 5, 5, 7 (late), 5, 10 (one missing), 5.
	tr.observe(idKey{id: 0x200}, start) // no period configured: ignored

	if got := counterValue(t, framesLate, iface, "100"); got != 1 {
		t.Errorf("late = %v, want 1", got)
	}
	if got := counterValue(t, framesMissedEstimated, iface, "100"); got != 1 {
		t.Errorf("estimated missed = %v, want 1", got)
	}
	var m dto.Metric
	if err := intervalRatio.WithLabelValues(iface, "100").(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetHistogram().GetSampleCount(); got != 6 {
		t.Errorf("interval samples = %d, want 6", got)
	}
}

func counterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
