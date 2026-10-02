package sampler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type dockerSampler struct {
	container string
	docker    string // binary name/path
}

// NewDocker returns a sampler that runs
// `docker stats --no-stream --format "{{json .}}" <container>` and reports the
// container memory usage (MemUsage, e.g. "351.2MiB / 512MiB") as RSSBytes.
// Heap, FDs and sessions are nil. Note each call takes ~1-2 s (docker stats
// needs two CPU readings), so use an interval of at least a few seconds.
func NewDocker(container string) Sampler {
	return &dockerSampler{container: container, docker: "docker"}
}

func (d *dockerSampler) Kind() string { return "docker" }

func (d *dockerSampler) Sample(ctx context.Context) (Point, error) {
	t := time.Now()
	cmd := exec.CommandContext(ctx, d.docker, "stats", "--no-stream", "--format", "{{json .}}", d.container)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Point{}, fmt.Errorf("docker stats %s: %w: %s", d.container, err, strings.TrimSpace(stderr.String()))
	}
	rss, err := parseDockerStatsLine(out)
	if err != nil {
		return Point{}, fmt.Errorf("docker stats %s: %w", d.container, err)
	}
	return Point{T: t, RSSBytes: fp(rss)}, nil
}

// parseDockerStatsLine extracts the memory usage in bytes from one
// `docker stats --format "{{json .}}"` line (the first non-empty line).
func parseDockerStatsLine(out []byte) (float64, error) {
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return 0, fmt.Errorf("empty output")
	}
	var st struct {
		MemUsage string `json:"MemUsage"`
	}
	if err := json.Unmarshal([]byte(line), &st); err != nil {
		return 0, fmt.Errorf("parse stats json: %w", err)
	}
	return ParseMemUsage(st.MemUsage)
}

// ParseMemUsage parses docker's MemUsage column ("351.2MiB / 512MiB") and
// returns the usage (left side) in bytes. Accepts binary (KiB, MiB, GiB, TiB)
// and decimal (kB, KB, MB, GB, TB) units and plain B.
func ParseMemUsage(s string) (float64, error) {
	used := s
	if i := strings.Index(s, "/"); i >= 0 {
		used = s[:i]
	}
	return ParseSize(strings.TrimSpace(used))
}

// ParseSize parses a docker/go-units human size like "1.5GiB", "900kB", "12B".
func ParseSize(s string) (float64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	mult, ok := sizeUnits[strings.ToLower(unit)]
	if !ok {
		return 0, fmt.Errorf("bad size unit %q in %q", unit, s)
	}
	return v * mult, nil
}

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"k": 1e3, "m": 1e6, "g": 1e9,
}
