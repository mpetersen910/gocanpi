// gocanpi — a CAN-FD telemetry collector.
//
// Opens a raw SocketCAN socket on one or more interfaces, decodes every frame,
// and exposes Prometheus counters over HTTP.
//
// Deliberately uses raw syscalls (golang.org/x/sys/unix) rather than a CAN
// library: the popular Go CAN libraries model the classic 8-byte frame and
// cannot represent the 64-byte CAN-FD frames this bench actually carries.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// SocketCAN constants
//
// These live in <linux/can.h>. x/sys/unix exports some but not all, so they're
// defined here to keep the decoding logic self-contained and greppable.
// ---------------------------------------------------------------------------

const (
	// Flag bits packed into the top of the 32-bit CAN ID field.
	canEFFFlag = 0x80000000 // extended (29-bit) identifier
	canRTRFlag = 0x40000000 // remote transmission request
	canERRFlag = 0x20000000 // this is an error frame, not a data frame

	// Masks to recover the actual identifier once the flags are stripped.
	canSFFMask = 0x000007FF // 11-bit standard ID
	canEFFMask = 0x1FFFFFFF // 29-bit extended ID
	canERRMask = 0x1FFFFFFF // error class bits live in the ID field

	// Frame sizes on the wire. The read length tells us which kind arrived.
	canMTU   = 16 // struct can_frame   — classic, up to 8 data bytes
	canFDMTU = 72 // struct canfd_frame — FD, up to 64 data bytes

	// Flags byte in a canfd_frame (offset 5).
	canFDBRS = 0x01 // bit rate switch — the data phase ran at the faster rate
	canFDESI = 0x02 // error state indicator — transmitter was error-passive
)

// Error classes, signalled in the ID field when canERRFlag is set.
// This is the most operationally interesting data on the bus: it's how you
// see a controller melting down before frames stop arriving entirely.
var errClasses = []struct {
	bit  uint32
	name string
}{
	{0x00000001, "tx_timeout"},
	{0x00000002, "lost_arbitration"},
	{0x00000004, "controller"},
	{0x00000008, "protocol"},
	{0x00000010, "transceiver"},
	{0x00000020, "no_ack"},
	{0x00000040, "bus_off"},
	{0x00000080, "bus_error"},
	{0x00000100, "restarted"},
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	framesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_received_total",
		Help: "CAN frames received, by interface, identifier and frame format.",
	}, []string{"iface", "can_id", "format"})

	// Same counts without the can_id label. Always safe to graph, never
	// explodes in cardinality — this is the one to build dashboards on.
	framesByIface = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_by_interface_total",
		Help: "CAN frames received per interface (no per-ID cardinality).",
	}, []string{"iface", "format"})

	bytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_payload_bytes_total",
		Help: "Total CAN payload bytes received, by interface.",
	}, []string{"iface"})

	errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_error_frames_total",
		Help: "CAN error frames received, by interface and error class.",
	}, []string{"iface", "class"})

	readErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_socket_read_errors_total",
		Help: "Failed reads from the CAN socket, by interface.",
	}, []string{"iface"})

	localDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "can_frames_local_dropped_total",
		Help: "Frames ignored because they were sent from this machine rather than heard on the wire, by interface.",
	}, []string{"iface"})

	lastFrame = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "can_last_frame_timestamp_seconds",
		Help: "Unix timestamp of the most recent frame, by interface. Staleness alarm.",
	}, []string{"iface"})

	distinctIDs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "can_distinct_ids",
		Help: "Distinct CAN identifiers seen, by interface.",
	}, []string{"iface"})

	payloadLen = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "can_payload_length_bytes",
		Help:    "Distribution of payload lengths. FD frames show up above 8.",
		Buckets: []float64{0, 1, 2, 4, 8, 12, 16, 24, 32, 48, 64},
	}, []string{"iface"})
)

// ---------------------------------------------------------------------------
// Frame decoding
// ---------------------------------------------------------------------------

// Frame is a decoded CAN or CAN-FD frame.
type Frame struct {
	ID       uint32 // identifier with flag bits already masked off
	Extended bool   // 29-bit rather than 11-bit
	RTR      bool   // remote transmission request
	IsError  bool   // error frame — ID field holds error class bits
	FD       bool   // arrived as a canfd_frame
	BRS      bool   // bit rate switch was set
	ESI      bool   // error state indicator was set
	Data     []byte
	RxTime   time.Time // when the kernel received the frame
}

// parseFrame decodes the raw bytes read from the socket.
//
// struct can_frame / canfd_frame, little-endian on aarch64:
//
//	offset 0..3  can_id   uint32   (flag bits in the top three)
//	offset 4     len      uint8    (0..8 classic, 0..64 FD)
//	offset 5     flags    uint8    (FD only: BRS, ESI)
//	offset 6..7  reserved
//	offset 8..   data
func parseFrame(buf []byte, n int) (Frame, error) {
	if n != canMTU && n != canFDMTU {
		return Frame{}, fmt.Errorf("unexpected frame size %d", n)
	}

	raw := binary.LittleEndian.Uint32(buf[0:4])
	length := int(buf[4])

	f := Frame{
		Extended: raw&canEFFFlag != 0,
		RTR:      raw&canRTRFlag != 0,
		IsError:  raw&canERRFlag != 0,
		FD:       n == canFDMTU,
	}

	// Strip the flag bits. Which mask applies depends on the frame kind —
	// getting this wrong is the classic bug: the same logical message shows
	// up as two different IDs and your dashboard doubles.
	switch {
	case f.IsError:
		f.ID = raw & canERRMask
	case f.Extended:
		f.ID = raw & canEFFMask
	default:
		f.ID = raw & canSFFMask
	}

	if f.FD {
		f.BRS = buf[5]&canFDBRS != 0
		f.ESI = buf[5]&canFDESI != 0
	}

	// Guard against a length byte that doesn't fit the buffer we were given.
	max := n - 8
	if length > max {
		length = max
	}
	f.Data = buf[8 : 8+length]

	return f, nil
}

// format returns a low-cardinality label describing the frame kind.
func (f Frame) format() string {
	switch {
	case f.IsError:
		return "error"
	case f.RTR:
		return "rtr"
	case f.FD && f.BRS:
		return "fd_brs"
	case f.FD:
		return "fd"
	case f.Extended:
		return "ext"
	default:
		return "std"
	}
}

// ---------------------------------------------------------------------------
// Socket setup
// ---------------------------------------------------------------------------

// openCAN creates a raw SocketCAN socket bound to the named interface.
//
// Requires CAP_NET_RAW. In Kubernetes that means the pod needs NET_RAW in its
// securityContext capabilities (NET_ADMIN is only needed if the process also
// *configures* the interface, which this one does not).
func openCAN(iface string, wantFD, wantErrors bool) (int, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return -1, fmt.Errorf("interface %s: %w", iface, err)
	}

	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW, unix.CAN_RAW)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}

	// The single most important line for this bench. Without it the kernel
	// hands us 16-byte classic frames and silently DROPS every FD frame —
	// you'd see a suspiciously quiet bus and no error anywhere.
	if wantFD {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("enable FD frames: %w", err)
		}
	}

	// Ask for error frames too. Off by default, so bus-off events would
	// otherwise be invisible — the bus would just go quiet.
	if wantErrors {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_ERR_FILTER, canERRMask); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("enable error filter: %w", err)
		}
	}

	// Have the kernel stamp each frame as the driver hands it up. Timing
	// taken after Read returns would include however long this goroutine
	// took to get scheduled, which is the same size as the jitter we measure.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("enable timestamps: %w", err)
	}

	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind %s: %w", iface, err)
	}

	return fd, nil
}

// rxTime pulls the kernel's receive timestamp out of the control messages
// that arrive with a frame, falling back to now if there isn't one.
func rxTime(oob []byte) time.Time {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Now()
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_TIMESTAMPNS {
			continue
		}
		var ts unix.Timespec
		if _, err := binary.Decode(m.Data, binary.NativeEndian, &ts); err == nil {
			return time.Unix(ts.Unix())
		}
	}
	return time.Now()
}

// isLocal reports whether a frame was sent from this machine rather than
// heard on the wire. SocketCAN gives every socket on an interface a copy of
// what the others send there, and marks those copies with MSG_DONTROUTE.
func isLocal(recvflags int) bool {
	return recvflags&unix.MSG_DONTROUTE != 0
}

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

type reader struct {
	iface        string
	fd           int
	maxIDs       int
	perID        bool
	includeLocal bool
	mu           sync.Mutex
	seenIDs      map[uint32]bool
	monitors     map[idKey][]monitor
}

func (r *reader) run(ctx context.Context) {
	buf := make([]byte, canFDMTU)
	oob := make([]byte, unix.CmsgSpace(binary.Size(unix.Timespec{})))
	r.seenIDs = make(map[uint32]bool)

	log.Printf("reading %s", r.iface)

	for {
		// Bail out if we're shutting down. Closing the socket in main does
		// not wake a Recvmsg that is already blocked; on a busy bus the next
		// frame does, and on a quiet one main exits without waiting for us.
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, oobn, recvflags, _, err := unix.Recvmsg(r.fd, buf, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue // interrupted by a signal, retry
			}
			if ctx.Err() != nil {
				return // expected: socket closed during shutdown
			}
			readErrors.WithLabelValues(r.iface).Inc()
			log.Printf("%s: read: %v", r.iface, err)
			time.Sleep(100 * time.Millisecond) // don't spin on a dead socket
			continue
		}

		// An outside listener only hears the wire. Frames this machine sent
		// are heard on the wire by the other interface's reader instead.
		if !r.includeLocal && isLocal(recvflags) {
			localDropped.WithLabelValues(r.iface).Inc()
			continue
		}

		f, err := parseFrame(buf, n)
		if err != nil {
			readErrors.WithLabelValues(r.iface).Inc()
			continue
		}
		f.RxTime = rxTime(oob[:oobn])

		r.record(f)
	}
}

func (r *reader) record(f Frame) {
	lastFrame.WithLabelValues(r.iface).Set(float64(time.Now().UnixNano()) / 1e9)

	if f.IsError {
		// One frame can carry several error classes at once.
		for _, ec := range errClasses {
			if f.ID&ec.bit != 0 {
				errorsTotal.WithLabelValues(r.iface, ec.name).Inc()
			}
		}
		framesByIface.WithLabelValues(r.iface, "error").Inc()
		return
	}

	format := f.format()
	framesByIface.WithLabelValues(r.iface, format).Inc()
	bytesTotal.WithLabelValues(r.iface).Add(float64(len(f.Data)))
	payloadLen.WithLabelValues(r.iface).Observe(float64(len(f.Data)))

	k := idKey{id: f.ID, extended: f.Extended}
	for _, m := range r.monitors[k] {
		m.observe(f)
	}

	if !r.perID {
		return
	}

	// Cardinality guard.
	//
	// Every distinct can_id creates a new Prometheus time series. A bus with
	// a few dozen IDs is fine; `cangen -I 100:7FF` emits up to 1792 of them
	// and will happily bloat your TSDB. Past the cap, everything collapses
	// into one "overflow" series so the metric stays useful and bounded.
	r.mu.Lock()
	known := r.seenIDs[f.ID]
	if !known {
		if len(r.seenIDs) >= r.maxIDs {
			r.mu.Unlock()
			framesTotal.WithLabelValues(r.iface, "overflow", format).Inc()
			return
		}
		r.seenIDs[f.ID] = true
		distinctIDs.WithLabelValues(r.iface).Set(float64(len(r.seenIDs)))
	}
	r.mu.Unlock()

	framesTotal.WithLabelValues(r.iface, k.label(), format).Inc()
}

// idKey identifies a message. The frame kind is part of it because a
// standard and an extended frame can carry the same numeric ID.
type idKey struct {
	id       uint32
	extended bool
}

// label is the can_id label value. Hex, zero-padded — sorts correctly and
// matches how candump prints IDs.
func (k idKey) label() string {
	if k.extended {
		return fmt.Sprintf("%08X", k.id)
	}
	return fmt.Sprintf("%03X", k.id)
}

// parseID reads a CAN ID written in hex the way candump prints it: up to
// three digits is a standard ID, more is an extended one. A 0x prefix is
// allowed.
func parseID(s string) (idKey, error) {
	digits := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	v, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return idKey{}, fmt.Errorf("%q is not a hex CAN ID", s)
	}
	k := idKey{id: uint32(v), extended: len(digits) > 3}
	if k.extended && v > canEFFMask {
		return idKey{}, fmt.Errorf("%q is above 1FFFFFFF, the largest extended CAN ID", s)
	}
	if !k.extended && v > canSFFMask {
		return idKey{}, fmt.Errorf("%q is above 7FF, the largest standard CAN ID", s)
	}
	return k, nil
}

// ---------------------------------------------------------------------------

func main() {
	var (
		ifaces = flag.String("ifaces", "can1", "comma-separated CAN interfaces to read")
		listen = flag.String("listen", ":9100", "address for the metrics endpoint")
		perID  = flag.Bool("per-id", true, "emit per-CAN-ID metrics")
		maxIDs = flag.Int("max-ids", 256, "cap on distinct IDs before collapsing to 'overflow'")
		noFD   = flag.Bool("no-fd", false, "disable CAN-FD reception (classic frames only)")
		noErrs = flag.Bool("no-errors", false, "don't subscribe to error frames")

		catalogPath  = flag.String("catalog", "", "YAML message catalog: each ID's name, type (periodic or event), limits and counter")
		includeLocal = flag.Bool("include-local", false, "also count frames sent from this machine, not just those heard on the wire")
	)
	flag.Parse()

	catalog, err := loadCatalog(*catalogPath)
	if err != nil {
		log.Fatalf("-catalog: %v", err)
	}
	if *catalogPath != "" {
		log.Printf("catalog: %d messages from %s", len(catalog), *catalogPath)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fds []int
	for _, name := range splitAndTrim(*ifaces) {
		fd, err := openCAN(name, !*noFD, !*noErrs)
		if err != nil {
			log.Fatalf("open %s: %v", name, err)
		}
		fds = append(fds, fd)

		localDropped.WithLabelValues(name)
		r := &reader{
			iface: name, fd: fd, perID: *perID, maxIDs: *maxIDs,
			includeLocal: *includeLocal,
			monitors:     newMonitors(name, catalog),
		}
		go r.run(ctx)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	// Liveness/readiness target for the Kubernetes probes.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{Addr: *listen, Handler: mux}

	go func() {
		log.Printf("metrics on %s/metrics", *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	// SIGTERM is what Kubernetes sends before it kills the pod. Handling it
	// is the difference between a clean rolling update and a dropped socket.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down")
	cancel()
	for _, fd := range fds {
		unix.Close(fd)
	}

	shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = srv.Shutdown(shutdownCtx)
}

func splitAndTrim(s string) []string {
	var out []string
	for _, part := range splitComma(s) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trim(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trim(s[start:]))
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
