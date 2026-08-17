// gencan — a deterministic CAN-FD traffic generator.
//
// Companion to the collector. Where the collector reads, this writes: it
// emits a small fixed schedule of messages at distinct periods, the way a
// real vehicle bus behaves. That matters for the dashboard — `cangen`'s
// uniform random IDs produce a flat, uninformative graph, while a handful of
// identifiable messages at known rates makes it obvious when something is
// late, missing, or bursting.
//
// Payloads carry a rolling counter so a consumer can detect dropped frames.
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	canEFFFlag = 0x80000000
	canMTU     = 16
	canFDMTU   = 72
	canFDBRS   = 0x01
)

// validFDLengths — CAN-FD does NOT allow arbitrary payload sizes. Above 8
// bytes only these are legal, because the DLC field encodes a size class
// rather than a count. Writing a frame with len=10 fails at the socket with
// a bare EINVAL and no hint as to why.
var validFDLengths = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64}

func padToValidFD(n int) int {
	for _, v := range validFDLengths {
		if v >= n {
			return v
		}
	}
	return 64
}

// message is one entry in the transmit schedule.
type message struct {
	id       uint32
	period   time.Duration
	length   int
	fd       bool
	brs      bool
	extended bool
	label    string
	// fill writes the payload. seq is the per-message frame counter.
	fill func(buf []byte, seq uint64)
}

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
		label: "diag_extended",
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

// openCAN opens a raw SocketCAN socket for transmission.
func openCAN(iface string) (int, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW, unix.CAN_RAW)
	if err != nil {
		return -1, err
	}
	// Needed to WRITE FD frames as well as read them — without it the
	// kernel rejects any write longer than 16 bytes.
	if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// encode builds the on-wire struct can_frame / canfd_frame.
func encode(m message, seq uint64) []byte {
	size := canMTU
	length := m.length
	if m.fd {
		size = canFDMTU
		length = padToValidFD(length)
	}

	buf := make([]byte, size)

	id := m.id
	if m.extended {
		id |= canEFFFlag
	}
	binary.LittleEndian.PutUint32(buf[0:4], id)
	buf[4] = byte(length)
	if m.fd && m.brs {
		buf[5] = canFDBRS
	}

	m.fill(buf[8:8+m.length], seq)
	return buf
}

func main() {
	var (
		iface = flag.String("iface", "can0", "CAN interface to transmit on")
		scale = flag.Float64("scale", 1.0, "period multiplier; <1 is faster, >1 slower")
		burst = flag.Duration("burst-every", 0, "periodically emit a rapid burst (0 disables)")
		quiet = flag.Bool("quiet", false, "suppress the periodic transmit summary")
	)
	flag.Parse()

	fd, err := openCAN(*iface)
	if err != nil {
		log.Fatalf("open %s: %v", *iface, err)
	}
	defer unix.Close(fd)

	// One socket shared across goroutines, so writes need serialising.
	var mu sync.Mutex
	var sent, failed uint64

	write := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		if _, err := unix.Write(fd, b); err != nil {
			failed++
			// ENOBUFS means the interface tx queue is full — the bus can't
			// carry what we're asking for. Real backpressure, worth seeing.
			if failed%100 == 1 {
				log.Printf("write: %v", err)
			}
			return
		}
		sent++
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for _, m := range schedule {
		wg.Add(1)
		go func(m message) {
			defer wg.Done()
			period := time.Duration(float64(m.period) * *scale)
			ticker := time.NewTicker(period)
			defer ticker.Stop()

			var seq uint64
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					write(encode(m, seq))
					seq++
				}
			}
		}(m)
	}

	// Optional burst generator: 200 frames as fast as the bus accepts them.
	// Gives the dashboard a spike to look at, and exercises the tx queue.
	if *burst > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(*burst)
			defer ticker.Stop()
			m := message{
				id: 0x7FF, length: 8, label: "burst",
				fill: func(b []byte, seq uint64) {
					binary.BigEndian.PutUint64(b[0:8], seq)
				},
			}
			var seq uint64
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					log.Printf("burst")
					for i := 0; i < 200; i++ {
						write(encode(m, seq))
						seq++
					}
				}
			}
		}()
	}

	if !*quiet {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					mu.Lock()
					log.Printf("sent=%d failed=%d", sent, failed)
					mu.Unlock()
				}
			}
		}()
	}

	log.Printf("transmitting on %s: %d messages, scale=%.2f", *iface, len(schedule), *scale)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("stopping")
	close(stop)
	wg.Wait()
}