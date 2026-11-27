package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// The catalog is what a spec sheet (a DBC file, the NMEA 2000 PGN list, or
// whoever owns the bus) says about each message: what it is called, how it
// is sent, and where its rolling counter lives. colcan checks every frame of
// a cataloged message against its entry. Messages not in the catalog only
// get the generic per-ID metrics.

type messageType string

const (
	typePeriodic messageType = "periodic" // sent on a fixed cycle
	typeEvent    messageType = "event"    // sent on change, with a heartbeat
)

// catalogFile mirrors the YAML. Durations and byte ranges stay strings here
// and are parsed per entry, so an error can name the message it came from.
type catalogFile struct {
	Messages []catalogEntry `yaml:"messages"`
}

type catalogEntry struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	Period     string `yaml:"period"`
	MinGap     string `yaml:"min_gap"`
	MaxSilence string `yaml:"max_silence"`
	Counter    string `yaml:"counter"`
}

// messageSpec is one validated catalog entry.
type messageSpec struct {
	key        idKey
	name       string
	kind       messageType
	period     time.Duration // periodic
	minGap     time.Duration // event; 0 skips the too-soon check
	maxSilence time.Duration // event
	counter    *counterField // optional
}

// labels are the label values every catalog metric carries.
func (s messageSpec) labels(iface string) []string {
	return []string{iface, s.key.label(), s.name}
}

var catalogLabels = []string{"iface", "can_id", "message"}

// loadCatalog reads the catalog at path. An empty path means no catalog.
func loadCatalog(path string) (map[idKey]messageSpec, error) {
	if path == "" {
		return map[idKey]messageSpec{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseCatalog(f)
}

func parseCatalog(r io.Reader) (map[idKey]messageSpec, error) {
	dec := yaml.NewDecoder(r)
	// A misspelled key would otherwise be dropped without a word, and its
	// check would silently never run.
	dec.KnownFields(true)

	var file catalogFile
	if err := dec.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("catalog is empty")
		}
		return nil, err
	}

	specs := map[idKey]messageSpec{}
	names := map[string]bool{}
	for i, e := range file.Messages {
		s, err := e.spec()
		if err != nil {
			return nil, fmt.Errorf("message %d (%s): %w", i+1, e.describe(), err)
		}
		if _, dup := specs[s.key]; dup {
			return nil, fmt.Errorf("message %d: ID %s is listed twice", i+1, s.key.label())
		}
		if names[s.name] {
			return nil, fmt.Errorf("message %d: name %q is used twice", i+1, s.name)
		}
		specs[s.key] = s
		names[s.name] = true
	}
	return specs, nil
}

func (e catalogEntry) describe() string {
	if e.Name != "" {
		return e.Name
	}
	return "id " + e.ID
}

func (e catalogEntry) spec() (messageSpec, error) {
	k, err := parseID(e.ID)
	if err != nil {
		return messageSpec{}, err
	}
	if e.Name == "" {
		return messageSpec{}, errors.New("name is required")
	}
	s := messageSpec{key: k, name: e.Name, kind: messageType(e.Type)}

	switch s.kind {
	case typePeriodic:
		if e.MinGap != "" || e.MaxSilence != "" {
			return messageSpec{}, errors.New("min_gap and max_silence only apply to event messages")
		}
		if s.period, err = parseDuration("period", e.Period, true); err != nil {
			return messageSpec{}, err
		}
	case typeEvent:
		if e.Period != "" {
			return messageSpec{}, errors.New("period only applies to periodic messages")
		}
		if s.minGap, err = parseDuration("min_gap", e.MinGap, false); err != nil {
			return messageSpec{}, err
		}
		if s.maxSilence, err = parseDuration("max_silence", e.MaxSilence, true); err != nil {
			return messageSpec{}, err
		}
		if s.maxSilence <= s.minGap {
			return messageSpec{}, errors.New("max_silence must be longer than min_gap")
		}
	case "":
		return messageSpec{}, errors.New("type is required: periodic or event")
	default:
		return messageSpec{}, fmt.Errorf("type %q: want periodic or event", e.Type)
	}

	if e.Counter != "" {
		c, err := parseByteRange(e.Counter)
		if err != nil {
			return messageSpec{}, fmt.Errorf("counter: %w", err)
		}
		s.counter = &c
	}
	return s, nil
}

// parseDuration reads a field like "5ms". A required field must be present
// and positive; an optional one may be absent, which reads as 0.
func parseDuration(field, s string, required bool) (time.Duration, error) {
	if s == "" {
		if required {
			return 0, fmt.Errorf("%s is required", field)
		}
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", field, err)
	}
	if d < 0 || (required && d == 0) {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return d, nil
}
