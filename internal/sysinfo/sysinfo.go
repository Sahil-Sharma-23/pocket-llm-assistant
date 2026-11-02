// Package sysinfo reads memory information that Android 16 still exposes to
// apps: /proc/meminfo and /proc/<pid>/status. (/proc/stat, /proc/loadavg and
// thermal sysfs are denied by SELinux and deliberately not used.)
package sysinfo

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ErrUnsupported is returned on platforms without /proc (e.g. macOS dev).
var ErrUnsupported = errors.New("sysinfo: /proc not available on this platform")

// Mem is a /proc/meminfo snapshot in megabytes.
type Mem struct {
	TotalMB     int64 `json:"total_mb"`
	AvailableMB int64 `json:"available_mb"`
	SwapTotalMB int64 `json:"swap_total_mb"`
	SwapFreeMB  int64 `json:"swap_free_mb"`
}

// Proc is a /proc/<pid>/status snapshot in megabytes.
type Proc struct {
	PID     int   `json:"pid"`
	RSSMB   int64 `json:"rss_mb"`
	AnonMB  int64 `json:"rss_anon_mb"` // non-reclaimable
	FileMB  int64 `json:"rss_file_mb"` // mmap'd files (model weights); reclaimable
	PeakMB  int64 `json:"peak_mb"`
	SwapMB  int64 `json:"swap_mb"`
	Threads int64 `json:"threads"`
}

// ReadMem reads /proc/meminfo.
func ReadMem() (Mem, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Mem{}, ErrUnsupported
		}
		return Mem{}, err
	}
	defer f.Close()
	return parseMem(f)
}

func parseMem(r io.Reader) (Mem, error) {
	kv, err := parseKV(r)
	if err != nil {
		return Mem{}, err
	}
	avail, ok := kv["MemAvailable"]
	if !ok {
		return Mem{}, fmt.Errorf("sysinfo: MemAvailable missing")
	}
	return Mem{
		TotalMB:     kv["MemTotal"] / 1024,
		AvailableMB: avail / 1024,
		SwapTotalMB: kv["SwapTotal"] / 1024,
		SwapFreeMB:  kv["SwapFree"] / 1024,
	}, nil
}

// ReadProc reads /proc/<pid>/status.
func ReadProc(pid int) (Proc, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, serr := os.Stat("/proc/self"); serr != nil {
				return Proc{}, ErrUnsupported
			}
		}
		return Proc{}, err
	}
	defer f.Close()
	p, err := parseProc(f)
	p.PID = pid
	return p, err
}

func parseProc(r io.Reader) (Proc, error) {
	kv, err := parseKV(r)
	if err != nil {
		return Proc{}, err
	}
	return Proc{
		RSSMB:   kv["VmRSS"] / 1024,
		AnonMB:  kv["RssAnon"] / 1024,
		FileMB:  kv["RssFile"] / 1024,
		PeakMB:  kv["VmHWM"] / 1024,
		SwapMB:  kv["VmSwap"] / 1024,
		Threads: kv["Threads"],
	}, nil
}

// parseKV parses "Key:   123 kB" lines into kB (or raw numbers).
func parseKV(r io.Reader) (map[string]int64, error) {
	out := map[string]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(k)] = n
	}
	return out, sc.Err()
}
