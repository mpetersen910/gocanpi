package main

import (
	"testing"
	"time"
)

func TestEventMonitor(t *testing.T) {
	const iface = "test-event"
	s := messageSpec{
		key: idKey{id: 0x400}, name: "cabin_temp", kind: typeEvent,
		minGap: 90 * time.Millisecond, maxSilence: 7500 * time.Millisecond,
	}
	m := newEventMonitor(iface, s)

	// Gaps: 1840, 1280, 5000, 100 (min-gapped, fine), 50 (too soon), 8000 (overdue).
	for _, ms := range []int{0, 1840, 3120, 8120, 8220, 8270, 16270} {
		m.observe(frameAt(ms))
	}

	labels := s.labels(iface)
	if got := counterValue(t, framesTooSoon, labels...); got != 1 {
		t.Errorf("too soon = %v, want 1", got)
	}
	if got := counterValue(t, framesOverdue, labels...); got != 1 {
		t.Errorf("overdue = %v, want 1", got)
	}
	if got := histogramCount(t, intervalSeconds, labels...); got != 6 {
		t.Errorf("interval samples = %d, want 6", got)
	}
}

func TestEventMonitorWithoutMinGap(t *testing.T) {
	const iface = "test-event-nogap"
	s := messageSpec{key: idKey{id: 0x401}, name: "door", kind: typeEvent, maxSilence: time.Second}
	m := newEventMonitor(iface, s)
	for _, ms := range []int{0, 1, 2} {
		m.observe(frameAt(ms))
	}
	if got := counterValue(t, framesTooSoon, s.labels(iface)...); got != 0 {
		t.Errorf("too soon = %v with no min_gap, want 0", got)
	}
}

func TestNewMonitors(t *testing.T) {
	const iface = "test-monitors"
	field := counterField{offset: 2, width: 1}
	periodic := messageSpec{key: idKey{id: 0x200}, name: "slow_status", kind: typePeriodic, period: 50 * time.Millisecond}
	event := messageSpec{key: idKey{id: 0x402}, name: "cabin_temp_2", kind: typeEvent, maxSilence: 7500 * time.Millisecond, counter: &field}

	before := time.Now()
	monitors := newMonitors(iface, map[idKey]messageSpec{periodic.key: periodic, event.key: event})

	kinds := func(ms []monitor) (names []string) {
		for _, m := range ms {
			switch m.(type) {
			case *lastSeenMonitor:
				names = append(names, "last-seen")
			case *periodicMonitor:
				names = append(names, "periodic")
			case *eventMonitor:
				names = append(names, "event")
			case *counterMonitor:
				names = append(names, "counter")
			}
		}
		return names
	}
	if got := kinds(monitors[periodic.key]); len(got) != 2 || got[0] != "last-seen" || got[1] != "periodic" {
		t.Errorf("periodic message monitors = %v, want [last-seen periodic]", got)
	}
	if got := kinds(monitors[event.key]); len(got) != 3 || got[0] != "last-seen" || got[1] != "event" || got[2] != "counter" {
		t.Errorf("event message monitors = %v, want [last-seen event counter]", got)
	}

	// Last-seen starts at startup, then follows the newest frame.
	labels := event.labels(iface)
	if got := gaugeValue(t, lastSeen, labels...); got < unixSeconds(before)-1 {
		t.Errorf("last seen starts at %v, want about now", got)
	}
	arrival := frameAt(1500, 0, 0, 0)
	for _, m := range monitors[event.key] {
		m.observe(arrival)
	}
	if got, want := gaugeValue(t, lastSeen, labels...), unixSeconds(arrival.RxTime); got != want {
		t.Errorf("last seen = %v, want %v", got, want)
	}
}
