package sysinfo

import (
	"strings"
	"testing"
)

// Captured from a real phone (Android 16).
const meminfo = `MemTotal:        5614452 kB
MemFree:          135920 kB
MemAvailable:    3201732 kB
Cached:          3064416 kB
SwapTotal:       8388604 kB
SwapFree:        7308028 kB
`

const status = `Name:	llama-server
Threads:	9
VmHWM:	 3011996 kB
VmRSS:	 2483408 kB
RssAnon:	   50352 kB
RssFile:	 2432624 kB
VmSwap:	   91840 kB
`

func TestParseMem(t *testing.T) {
	m, err := parseMem(strings.NewReader(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	want := Mem{TotalMB: 5482, AvailableMB: 3126, SwapTotalMB: 8191, SwapFreeMB: 7136}
	if m != want {
		t.Errorf("got %+v, want %+v", m, want)
	}
	if _, err := parseMem(strings.NewReader("MemTotal: 1 kB\n")); err == nil {
		t.Error("expected error when MemAvailable missing")
	}
}

func TestParseProc(t *testing.T) {
	p, err := parseProc(strings.NewReader(status))
	if err != nil {
		t.Fatal(err)
	}
	if p.RSSMB != 2425 || p.AnonMB != 49 || p.FileMB != 2375 || p.PeakMB != 2941 || p.Threads != 9 || p.SwapMB != 89 {
		t.Errorf("got %+v", p)
	}
}
