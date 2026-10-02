package sampler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of utime/stime in /proc/<pid>/stat. It is
// 100 on every Linux ABI (reading sysconf(_SC_CLK_TCK) would need cgo).
const clockTicks = 100

// parseProcStat extracts utime+stime (fields 14 and 15) from a Linux
// /proc/<pid>/stat line. The comm field (2) may contain spaces and
// parentheses, so fields are counted from the last ')'.
func parseProcStat(s string) (time.Duration, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed /proc stat")
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); utime (14) is f[11], stime (15) is f[12].
	if len(f) < 13 {
		return 0, fmt.Errorf("malformed /proc stat: %d fields", len(f))
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64)
	st, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("malformed /proc stat times")
	}
	return time.Duration(u+st) * time.Second / clockTicks, nil
}

// parsePSTime parses the cumulative CPU time printed by `ps -o time=`:
// [[dd-]hh:]mm:ss[.ff].
func parsePSTime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty ps time")
	}
	orig := s
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		v, err := strconv.ParseFloat(d, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("bad ps time %q", orig)
		}
		days, s = v, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad ps time %q", orig)
	}
	var secs float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("bad ps time %q", orig)
		}
		secs = secs*60 + v
	}
	secs += days * 86400
	return time.Duration(secs * float64(time.Second)), nil
}
