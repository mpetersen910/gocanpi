package main

import (
	"strings"
	"testing"
	"time"
)

const benchCatalog = `
messages:
  - id: "100"
    name: fast_counter
    type: periodic
    period: 5ms
    counter: 0-3
  - id: 18DAF110
    name: diag_extended
    type: periodic
    period: 500ms
  - id: 400
    name: cabin_temp
    type: event
    min_gap: 90ms
    max_silence: 7.5s
    counter: 2-2
`

func TestParseCatalog(t *testing.T) {
	got, err := parseCatalog(strings.NewReader(benchCatalog))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d messages, want 3", len(got))
	}

	fast := got[idKey{id: 0x100}]
	if fast.name != "fast_counter" || fast.kind != typePeriodic || fast.period != 5*time.Millisecond {
		t.Errorf("fast_counter = %+v", fast)
	}
	if fast.counter == nil || *fast.counter != (counterField{offset: 0, width: 4}) {
		t.Errorf("fast_counter counter = %+v, want bytes 0-3", fast.counter)
	}

	if diag, ok := got[idKey{id: 0x18DAF110, extended: true}]; !ok || diag.counter != nil {
		t.Errorf("diag_extended = %+v, %v; want an extended ID with no counter", diag, ok)
	}

	// Unquoted, so YAML reads it as a number; it must still be taken as hex.
	temp := got[idKey{id: 0x400}]
	if temp.kind != typeEvent || temp.minGap != 90*time.Millisecond || temp.maxSilence != 7500*time.Millisecond {
		t.Errorf("cabin_temp = %+v", temp)
	}
}

func TestParseCatalogErrors(t *testing.T) {
	cases := map[string]string{
		"empty file":              ``,
		"unknown key":             "messages:\n  - {id: \"100\", name: a, type: periodic, period: 5ms, perod: 5ms}\n",
		"no name":                 "messages:\n  - {id: \"100\", type: periodic, period: 5ms}\n",
		"no type":                 "messages:\n  - {id: \"100\", name: a, period: 5ms}\n",
		"unknown type":            "messages:\n  - {id: \"100\", name: a, type: cyclic, period: 5ms}\n",
		"periodic no period":      "messages:\n  - {id: \"100\", name: a, type: periodic}\n",
		"period of zero":          "messages:\n  - {id: \"100\", name: a, type: periodic, period: 0s}\n",
		"event field on periodic": "messages:\n  - {id: \"100\", name: a, type: periodic, period: 5ms, max_silence: 1s}\n",
		"event no max_silence":    "messages:\n  - {id: \"400\", name: a, type: event, min_gap: 90ms}\n",
		"period on event":         "messages:\n  - {id: \"400\", name: a, type: event, period: 5ms, max_silence: 1s}\n",
		"silence under gap":       "messages:\n  - {id: \"400\", name: a, type: event, min_gap: 2s, max_silence: 1s}\n",
		"bad duration":            "messages:\n  - {id: \"100\", name: a, type: periodic, period: fast}\n",
		"bad id":                  "messages:\n  - {id: \"800\", name: a, type: periodic, period: 5ms}\n",
		"bad counter":             "messages:\n  - {id: \"100\", name: a, type: periodic, period: 5ms, counter: 0-4}\n",
		"duplicate id": "messages:\n  - {id: \"100\", name: a, type: periodic, period: 5ms}\n" +
			"  - {id: \"0x100\", name: b, type: periodic, period: 5ms}\n",
		"duplicate name": "messages:\n  - {id: \"100\", name: a, type: periodic, period: 5ms}\n" +
			"  - {id: \"200\", name: a, type: periodic, period: 5ms}\n",
	}
	for name, yamlText := range cases {
		if _, err := parseCatalog(strings.NewReader(yamlText)); err == nil {
			t.Errorf("%s: parseCatalog succeeded, want an error", name)
		}
	}
}

func TestLoadCatalogWithoutPath(t *testing.T) {
	got, err := loadCatalog("")
	if err != nil || len(got) != 0 {
		t.Errorf("loadCatalog(\"\") = %v, %v; want an empty catalog", got, err)
	}
}
