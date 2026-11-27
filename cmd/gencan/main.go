// gencan — a deterministic CAN-FD traffic generator.
//
// Companion to the collector. Where the collector reads, this writes. It runs
// in one of two modes:
//
//   - standard: a small fixed schedule of messages at distinct periods, the
//     way a real vehicle bus behaves. That matters for the dashboard —
//     `cangen`'s uniform random IDs produce a flat, uninformative graph, while
//     a handful of identifiable messages at known rates makes it obvious when
//     something is late, missing, or bursting. Payloads carry a rolling
//     counter so a consumer can detect dropped frames.
//   - burst: pulses of a single ID, each sent as fast as the bus accepts it.
//     Run it alongside a standard instance to load the tx queue or the bus.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	var (
		mode  = flag.String("mode", "standard", "standard: the periodic schedule; burst: pulses of one ID")
		iface = flag.String("iface", "can0", "CAN interface to transmit on")
		quiet = flag.Bool("quiet", false, "suppress the periodic transmit summary")

		scale = flag.Float64("scale", 1.0, "standard mode: period multiplier; <1 is faster, >1 slower")

		burstID    uint32 = 0x7FF
		burstCount        = flag.Int("burst-count", 200, "burst mode: frames per pulse")
		burstEvery        = flag.Duration("burst-every", 60*time.Second, "burst mode: time between pulses (0 sends them back to back)")
	)
	flag.Func("burst-id", "burst mode: standard CAN ID to send, in hex (default 7FF)", func(s string) error {
		id, err := parseStandardID(s)
		if err != nil {
			return err
		}
		burstID = id
		return nil
	})
	flag.Parse()

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
	default:
		log.Fatalf("unknown -mode %q (want standard or burst)", *mode)
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

// checkFlags rejects an unknown mode, and flags that belong to the other
// mode. An old -burst-every on a standard instance would otherwise be
// silently ignored and its burst would just stop happening.
func checkFlags(mode string, set map[string]bool) error {
	var foreign []string
	switch mode {
	case "standard":
		foreign = []string{"burst-id", "burst-count", "burst-every"}
	case "burst":
		foreign = []string{"scale"}
	default:
		return fmt.Errorf("unknown -mode %q (want standard or burst)", mode)
	}
	for _, name := range foreign {
		if set[name] {
			return fmt.Errorf("-%s doesn't apply to -mode=%s", name, mode)
		}
	}
	return nil
}
