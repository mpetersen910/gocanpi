package main

import (
	"encoding/binary"
	"log"
	"math"
	"sync"
	"time"
)

// The schedule. Periods are deliberately different so the Grafana panels
// show distinguishable rates rather than one undifferentiated blob.
var schedule = []message{
	{
		id: 0x100, period: 10 * time.Millisecond, length: 8,
		label: "fast_counter",
		fill: func(b []byte, seq uint64) {
			// Rolling counter — a gap in this sequence is a dropped frame.
			binary.BigEndian.PutUint32(b[0:4], uint32(seq))
			// Sawtooth, so a dashboard graph of byte 4 has visible shape.
			b[4] = byte(seq % 256)
			// Sine, quantised — looks like a real sensor reading.
			b[5] = byte(127 + 127*math.Sin(float64(seq)/50))
			b[6] = 0
			b[7] = checksum(b[0:7])
		},
	},
	{
		id: 0x200, period: 100 * time.Millisecond, length: 8,
		label: "slow_status",
		fill: func(b []byte, seq uint64) {
			binary.BigEndian.PutUint16(b[0:2], uint16(seq))
			b[2] = byte(seq % 4) // pretend state machine
			b[7] = checksum(b[0:7])
		},
	},
	{
		id: 0x300, period: 50 * time.Millisecond, length: 24,
		fd: true, brs: true,
		label: "fd_payload",
		fill: func(b []byte, seq uint64) {
			// Exercises the FD path specifically: >8 bytes with bit rate
			// switch set. If the collector's CAN_RAW_FD_FRAMES sockopt were
			// missing, THIS is the message that would silently vanish.
			binary.BigEndian.PutUint64(b[0:8], seq)
			for i := 8; i < len(b); i++ {
				b[i] = byte(i) ^ byte(seq)
			}
		},
	},
	{
		id: 0x18DAF110, period: 1000 * time.Millisecond, length: 8,
		extended: true,
		label:    "diag_extended",
		fill: func(b []byte, seq uint64) {
			// 29-bit ID, in the range real UDS diagnostics use. Verifies the
			// collector's extended-vs-standard masking is correct: get that
			// wrong and this shows up under a mangled identifier.
			b[0] = 0x02
			b[1] = 0x01
			binary.BigEndian.PutUint16(b[2:4], uint16(seq))
		},
	},
}

func checksum(b []byte) byte {
	var sum byte
	for _, v := range b {
		sum += v
	}
	return ^sum
}

// runStandard transmits every message in the schedule on its own ticker.
// A frame the queue won't take is dropped, and its sequence number is spent
// anyway, so receivers see the loss as a gap.
func runStandard(tx *transmitter, scale float64, stop <-chan struct{}, wg *sync.WaitGroup) {
	for _, m := range schedule {
		wg.Add(1)
		go func() {
			defer wg.Done()
			period := time.Duration(float64(m.period) * scale)
			ticker := time.NewTicker(period)
			defer ticker.Stop()

			var seq uint64
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					tx.send(encode(m, seq))
					seq++
				}
			}
		}()
	}
	log.Printf("standard: %d messages, scale=%.2f", len(schedule), scale)
}
