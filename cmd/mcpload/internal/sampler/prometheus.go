package sampler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PromNames are the Prometheus metric names mapped to each Point field.
// An empty name disables that field.
type PromNames struct {
	RSS, Heap, FDs, Sessions string
}

// DefaultPromNames returns the metric names exposed by prom-client (Node) and
// the demo servers.
func DefaultPromNames() PromNames {
	return PromNames{
		RSS:      "process_resident_memory_bytes",
		Heap:     "nodejs_heap_used_bytes",
		FDs:      "process_open_fds",
		Sessions: "mcp_active_sessions",
	}
}

type promSampler struct {
	url    string
	names  PromNames
	client *http.Client
}

// NewPrometheus returns a sampler that GETs url (a Prometheus text-format
// /metrics endpoint) and reads the named metrics, summing across label sets
// of the same metric name. Metrics not present yield nil fields.
func NewPrometheus(url string, names PromNames) Sampler {
	return &promSampler{url: url, names: names, client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *promSampler) Kind() string { return "prometheus" }

func (p *promSampler) Sample(ctx context.Context) (Point, error) {
	t := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return Point{}, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := p.client.Do(req)
	if err != nil {
		return Point{}, fmt.Errorf("scrape %s: %w", p.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Point{}, fmt.Errorf("scrape %s: HTTP %d", p.url, resp.StatusCode)
	}
	vals, err := ParsePromText(resp.Body)
	if err != nil {
		return Point{}, fmt.Errorf("scrape %s: %w", p.url, err)
	}
	get := func(name string) *float64 {
		if name == "" {
			return nil
		}
		if v, ok := vals[name]; ok {
			return fp(v)
		}
		return nil
	}
	pt := Point{T: t, RSSBytes: get(p.names.RSS), HeapBytes: get(p.names.Heap), OpenFDs: get(p.names.FDs), ActiveSessions: get(p.names.Sessions)}
	return pt, nil
}

// ParsePromText parses the Prometheus text exposition format and returns, for
// each metric name, the sum of its sample values across all label sets.
// Comment (#) and blank lines are skipped; NaN samples are ignored; an
// optional trailing timestamp is ignored. Histogram/summary series appear
// under their full sample names (e.g. foo_bucket, foo_sum).
func ParsePromText(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, rest, err := splitPromLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln, err)
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, fmt.Errorf("line %d: missing value", ln)
		}
		v, err := parsePromFloat(fields[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln, err)
		}
		if math.IsNaN(v) {
			continue
		}
		out[name] += v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// splitPromLine returns the metric name and the text after the label set.
func splitPromLine(line string) (name, rest string, err error) {
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	name = line[:i]
	if name == "" {
		return "", "", fmt.Errorf("missing metric name")
	}
	if i < len(line) && line[i] == '{' {
		// Skip label set, honouring quoted values with escapes.
		inQ := false
		j := i + 1
		for ; j < len(line); j++ {
			c := line[j]
			if inQ {
				if c == '\\' {
					j++
				} else if c == '"' {
					inQ = false
				}
				continue
			}
			if c == '"' {
				inQ = true
			} else if c == '}' {
				break
			}
		}
		if j >= len(line) {
			return "", "", fmt.Errorf("unterminated label set")
		}
		return name, line[j+1:], nil
	}
	return name, line[i:], nil
}

func parsePromFloat(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return v, nil
}
