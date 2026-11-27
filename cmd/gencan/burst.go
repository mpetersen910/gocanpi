package main

import (
	"encoding/binary"
	"log"
	"sync"
	"time"
)

// burstConfig describes the pulses burst mode sends.
type burstConfig struct {
	id    uint32        // standard (11-bit) CAN ID
	count int           // frames per pulse
	every time.Duration // time between pulses; 0 sends them back to back
}

// runBurst sends pulses of cfg.count frames, each pulse as fast as the bus
// accepts them. Sharing an interface with a standard instance, a pulse fills
// the tx queue, and the schedule's frames wait behind it or get dropped.
func runBurst(tx *transmitter, cfg burstConfig, stop <-chan struct{}, wg *sync.WaitGroup) {
	m := message{
		id: cfg.id, length: 8, label: "burst",
		fill: func(b []byte, seq uint64) {
			binary.BigEndian.PutUint64(b[0:8], seq)
		},
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		var tick <-chan time.Time
		if cfg.every > 0 {
			ticker := time.NewTicker(cfg.every)
			defer ticker.Stop()
			tick = ticker.C
		}

		var seq uint64
		for {
			if tick != nil {
				select {
				case <-stop:
					return
				case <-tick:
				}
			}

			_, _, retriesBefore := tx.counts()
			start := time.Now()
			for i := 0; i < cfg.count; i++ {
				tx.sendRetry(encode(m, seq), stop)
				seq++
				select {
				case <-stop:
					return
				default:
				}
			}
			// Back-to-back pulses would log many lines a second; the
			// periodic summary in main covers that case.
			if tick != nil {
				_, _, retriesAfter := tx.counts()
				log.Printf("burst: %d x %03X in %v, %d retries",
					cfg.count, cfg.id, time.Since(start).Round(time.Millisecond), retriesAfter-retriesBefore)
			}
		}
	}()
	log.Printf("burst: %d x %03X every %v", cfg.count, cfg.id, cfg.every)
}
