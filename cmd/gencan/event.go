package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Event mode simulates an on-change temperature sensor: it transmits when
// the reading has moved, not on a schedule. Two rules keep that sane, as on
// real buses: a minimum gap (inhibit time) so a noisy reading can't flood the
// bus, and a heartbeat so silence still proves the sensor is alive.

const (
	// How often the sensor measures. Internal only; what reaches the bus is
	// governed by the deadband, the minimum gap and the heartbeat.
	eventSample = 20 * time.Millisecond

	walkStep = 0.05  // °C, standard deviation of one sample's change
	walkHome = 21.0  // °C the reading drifts back toward
	walkPull = 0.002 // fraction of the distance home closed per sample
)

// eventConfig describes the simulated sensor.
type eventConfig struct {
	id        uint32        // standard (11-bit) CAN ID
	deadband  float64       // °C the reading must move before a frame is due
	minGap    time.Duration // shortest spacing between frames
	heartbeat time.Duration // longest silence; 0 disables
	seed      uint64
	quiet     bool
}

type sendReason int

const (
	noSend sendReason = iota
	sendFirst
	sendChange
	sendHeartbeat
)

func (r sendReason) String() string {
	switch r {
	case sendFirst:
		return "first"
	case sendChange:
		return "change"
	case sendHeartbeat:
		return "heartbeat"
	}
	return "none"
}

// sensorState is what the send decision depends on.
type sensorState struct {
	sentAny   bool
	lastSent  time.Time // when the last frame went out
	lastValue float64   // reading carried by the last frame
}

// shouldSend decides whether a reading taken at now goes on the bus. A
// change waits out the minimum gap; a heartbeat fires once the bus has been
// silent long enough, whatever the reading.
func shouldSend(s sensorState, now time.Time, value float64, cfg eventConfig) sendReason {
	if !s.sentAny {
		return sendFirst
	}
	since := now.Sub(s.lastSent)
	if math.Abs(value-s.lastValue) >= cfg.deadband && since >= cfg.minGap {
		return sendChange
	}
	if cfg.heartbeat > 0 && since >= cfg.heartbeat {
		return sendHeartbeat
	}
	return noSend
}

// encodeTemp writes the payload: temperature as a signed big-endian int16 in
// 0.1 °C units, then an 8-bit rolling counter. The rest stays zero.
func encodeTemp(b []byte, celsius float64, counter uint8) {
	binary.BigEndian.PutUint16(b[0:2], uint16(int16(math.Round(celsius*10))))
	b[2] = counter
}

// runEvent samples a random-walk temperature and transmits it on change,
// limited by the minimum gap and backed by the heartbeat.
func runEvent(tx *transmitter, cfg eventConfig, stop <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		rng := rand.New(rand.NewPCG(cfg.seed, 0))
		value := walkHome
		var reading float64
		m := message{
			id: cfg.id, length: 8, label: "temperature",
			fill: func(b []byte, seq uint64) { encodeTemp(b, reading, uint8(seq)) },
		}

		ticker := time.NewTicker(eventSample)
		defer ticker.Stop()

		var (
			state sensorState
			// When the reading first crossed the deadband but the minimum
			// gap held the frame back. Zero while nothing is waiting.
			pendingSince time.Time
			seq          uint64
		)
		for {
			var now time.Time
			select {
			case <-stop:
				return
			case now = <-ticker.C:
			}

			value += walkStep*rng.NormFloat64() + walkPull*(walkHome-value)
			crossed := state.sentAny && math.Abs(value-state.lastValue) >= cfg.deadband
			switch {
			case !crossed:
				pendingSince = time.Time{}
			case pendingSince.IsZero():
				pendingSince = now
			}

			reason := shouldSend(state, now, value, cfg)
			if reason == noSend {
				continue
			}

			reading = value
			tx.send(encode(m, seq))
			if !cfg.quiet {
				log.Printf("event: #%d %.1f C %s", seq, reading, describeSend(reason, state, now, pendingSince))
			}

			state = sensorState{sentAny: true, lastSent: now, lastValue: value}
			pendingSince = time.Time{}
			seq++
		}
	}()
	log.Printf("event: %03X, deadband %.2f C, min gap %v, heartbeat %v", cfg.id, cfg.deadband, cfg.minGap, cfg.heartbeat)
}

// describeSend is the tail of the per-frame log line, for example
// "after 100ms (change, held 60ms by min gap)".
func describeSend(reason sendReason, prev sensorState, now, pendingSince time.Time) string {
	if reason == sendFirst {
		return "(first)"
	}
	why := reason.String()
	if reason == sendChange {
		if held := now.Sub(pendingSince); held > 0 {
			why += fmt.Sprintf(", held %v by min gap", held.Round(time.Millisecond))
		}
	}
	return fmt.Sprintf("after %v (%s)", now.Sub(prev.lastSent).Round(time.Millisecond), why)
}
