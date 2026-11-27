// gencan — a deterministic CAN-FD traffic generator.
//
// Companion to the collector. Where the collector reads, this writes. It runs
// in one of three modes:
//
//   - standard: a small fixed schedule of messages at distinct periods, the
//     way a real vehicle bus behaves. That matters for the dashboard —
//     `cangen`'s uniform random IDs produce a flat, uninformative graph, while
//     a handful of identifiable messages at known rates makes it obvious when
//     something is late, missing, or bursting. Payloads carry a rolling
//     counter so a consumer can detect dropped frames.
//   - burst: pulses of a single ID, each sent as fast as the bus accepts it.
//     Run it alongside a standard instance to load the tx queue or the bus.
//   - event: an on-change temperature sensor. It transmits when the reading
//     moves, no faster than a minimum gap and no slower than a heartbeat, so
//     its frames arrive at irregular intervals.
package main

import (
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Default IDs for burst and event mode. Two senders must never share an ID,
// so neither may appear in the standard schedule.
const (
	defaultBurstID = 0x7FF
	defaultEventID = 0x400
)

func main() {
	var (
		mode  = flag.String("mode", "standard", "standard: the periodic schedule; burst: pulses of one ID; event: an on-change temperature sensor")
		iface = flag.String("iface", "can0", "CAN interface to transmit on")
		quiet = flag.Bool("quiet", false, "suppress the periodic transmit summary and event mode's per-frame log")

		scale = flag.Float64("scale", 1.0, "standard mode: period multiplier; <1 is faster, >1 slower")

		burstID    uint32 = defaultBurstID
		burstCount        = flag.Int("burst-count", 200, "burst mode: frames per pulse")
		burstEvery        = flag.Duration("burst-every", 60*time.Second, "burst mode: time between pulses (0 sends them back to back)")

		eventID        uint32 = defaultEventID
		eventDeadband         = flag.Float64("event-deadband", 0.5, "event mode: change in degrees C that makes a frame due")
		eventMinGap           = flag.Duration("event-min-gap", 100*time.Millisecond, "event mode: shortest spacing between frames")
		eventHeartbeat        = flag.Duration("event-heartbeat", 5*time.Second, "event mode: send at least this often, change or not (0 disables)")
		eventSeed             = flag.Uint64("event-seed", 1, "event mode: random walk seed, so runs are reproducible")
	)
	flag.Func("burst-id", "burst mode: standard CAN ID to send, in hex (default 7FF)", func(s string) error {
		id, err := parseStandardID(s)
		if err != nil {
			return err
		}
		burstID = id
		return nil
	})
	flag.Func("event-id", "event mode: standard CAN ID to send, in hex (default 400)", func(s string) error {
		id, err := parseStandardID(s)
		if err != nil {
			return err
		}
		eventID = id
		return nil
	})
	flag.Parse()
	// The per-frame event log is only useful with sub-second times.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if err := checkFlags(*mode, set); err != nil {
		log.Fatal(err)
	}
	if *burstCount < 1 {
		log.Fatal("-burst-count must be at least 1")
	}
	if *burstEvery < 0 {
		log.Fatal("-burst-every can't be negative")
	}
	if *eventDeadband <= 0 {
		log.Fatal("-event-deadband must be above 0")
	}
	if *eventMinGap < 0 || *eventHeartbeat < 0 {
		log.Fatal("-event-min-gap and -event-heartbeat can't be negative")
	}
	if *eventHeartbeat > 0 && *eventHeartbeat < *eventMinGap {
		log.Fatal("-event-heartbeat can't be shorter than -event-min-gap")
	}

	fd, err := openCAN(*iface)
	if err != nil {
		log.Fatalf("open %s: %v", *iface, err)
	}
	defer unix.Close(fd)

	tx := &transmitter{fd: fd}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	log.Printf("transmitting on %s", *iface)
	switch *mode {
	case "standard":
		runStandard(tx, *scale, stop, &wg)
	case "burst":
		runBurst(tx, burstConfig{id: burstID, count: *burstCount, every: *burstEvery}, stop, &wg)
	case "event":
		runEvent(tx, eventConfig{
			id: eventID, deadband: *eventDeadband, minGap: *eventMinGap,
			heartbeat: *eventHeartbeat, seed: *eventSeed, quiet: *quiet,
		}, stop, &wg)
	default:
		log.Fatalf("unknown -mode %q (want standard, burst or event)", *mode)
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
					sent, failed, retries := tx.counts()
					log.Printf("sent=%d failed=%d retries=%d", sent, failed, retries)
				}
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("stopping")
	close(stop)
	wg.Wait()
}

// modeFlags lists the flags that only mean something in one mode.
var modeFlags = map[string][]string{
	"standard": {"scale"},
	"burst":    {"burst-id", "burst-count", "burst-every"},
	"event":    {"event-id", "event-deadband", "event-min-gap", "event-heartbeat", "event-seed"},
}

// checkFlags rejects an unknown mode, and flags that belong to another mode.
// An old -burst-every on a standard instance would otherwise be silently
// ignored and its burst would just stop happening.
func checkFlags(mode string, set map[string]bool) error {
	if _, ok := modeFlags[mode]; !ok {
		return fmt.Errorf("unknown -mode %q (want standard, burst or event)", mode)
	}
	for _, other := range slices.Sorted(maps.Keys(modeFlags)) {
		if other == mode {
			continue
		}
		for _, name := range modeFlags[other] {
			if set[name] {
				return fmt.Errorf("-%s doesn't apply to -mode=%s", name, mode)
			}
		}
	}
	return nil
}
