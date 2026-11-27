package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// A monitor checks one message's frames against its catalog entry. Every
// reader builds its own set, so the state is per interface and only that
// reader's goroutine touches it.
type monitor interface {
	observe(f Frame)
}

// newMonitors builds the checks the catalog asks for on one interface:
// last-seen for every message, the check for its type, and a counter check
// if the entry names one.
func newMonitors(iface string, catalog map[idKey]messageSpec) map[idKey][]monitor {
	monitors := map[idKey][]monitor{}
	for k, s := range catalog {
		ms := []monitor{newLastSeenMonitor(iface, s)}
		switch s.kind {
		case typePeriodic:
			ms = append(ms, newPeriodicMonitor(iface, s))
		case typeEvent:
			ms = append(ms, newEventMonitor(iface, s))
		}
		if s.counter != nil {
			ms = append(ms, newCounterMonitor(iface, s))
		}
		monitors[k] = ms
	}
	return monitors
}

var lastSeen = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "can_frame_last_seen_timestamp_seconds",
	Help: "Arrival time of the latest frame of each catalog message. Starts at colcan's start time, so a message never heard still ages.",
}, catalogLabels)

// lastSeenMonitor is the liveness signal. Late and missing counts only move
// once a frame finally arrives; this one shows a dead sender while it is
// still dead.
type lastSeenMonitor struct {
	gauge prometheus.Gauge
}

func newLastSeenMonitor(iface string, s messageSpec) *lastSeenMonitor {
	g := lastSeen.WithLabelValues(s.labels(iface)...)
	g.Set(unixSeconds(time.Now()))
	return &lastSeenMonitor{gauge: g}
}

func (m *lastSeenMonitor) observe(f Frame) {
	m.gauge.Set(unixSeconds(f.RxTime))
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
