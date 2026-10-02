package sampler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const promFixture = `# HELP process_resident_memory_bytes Resident memory size in bytes.
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 87359488
process_open_fds 20 1700000000000
# TYPE nodejs_heap_used_bytes gauge
nodejs_heap_used_bytes 1.9207048e+07
mcp_active_sessions{replica="a",path="/m cp{x}"} 3
mcp_active_sessions{replica="b",note="quote \" brace }"} 4
http_request_duration_seconds_bucket{le="+Inf"} 12
weird_nan NaN
`

func TestParsePromText(t *testing.T) {
	m, err := ParsePromText(strings.NewReader(promFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"process_resident_memory_bytes":        87359488,
		"process_open_fds":                     20,
		"nodejs_heap_used_bytes":               19207048,
		"mcp_active_sessions":                  7,
		"http_request_duration_seconds_bucket": 12,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if _, ok := m["weird_nan"]; ok {
		t.Error("NaN sample should be skipped")
	}
}

func TestPrometheusSampler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(promFixture))
	}))
	defer srv.Close()
	names := DefaultPromNames()
	names.FDs = "" // disabled
	s := NewPrometheus(srv.URL, names)
	if s.Kind() != "prometheus" {
		t.Fatal(s.Kind())
	}
	p, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.RSSBytes == nil || *p.RSSBytes != 87359488 || p.HeapBytes == nil || *p.ActiveSessions != 7 || p.OpenFDs != nil || p.T.IsZero() {
		t.Fatalf("bad point %+v", p)
	}
}

func TestParseMemUsage(t *testing.T) {
	cases := map[string]float64{
		"351.2MiB / 512MiB": 351.2 * 1024 * 1024,
		"1.5GiB / 7.6GiB":   1.5 * 1024 * 1024 * 1024,
		"900kB / 1GB":       900e3,
		"512KiB / 1GiB":     512 * 1024,
		"123B / 1GiB":       123,
		"0B / 0B":           0,
		"2.5MB / 1GB":       2.5e6,
	}
	for in, want := range cases {
		got, err := ParseMemUsage(in)
		if err != nil || got != want {
			t.Errorf("ParseMemUsage(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "12XB / 1GiB"} {
		if _, err := ParseMemUsage(bad); err == nil {
			t.Errorf("ParseMemUsage(%q) should fail", bad)
		}
	}
}

func TestParseDockerStatsLine(t *testing.T) {
	line := `{"BlockIO":"0B / 0B","CPUPerc":"0.00%","Container":"x","ID":"abc","MemPerc":"1.07%","MemUsage":"84.27MiB / 7.66GiB","Name":"x","NetIO":"1kB / 0B","PIDs":"11"}` + "\r\n"
	got, err := parseDockerStatsLine([]byte(line))
	if err != nil || got != 84.27*1024*1024 {
		t.Fatalf("got %v %v", got, err)
	}
}

type fakeSampler struct{ n atomic.Int32 }

func (f *fakeSampler) Kind() string { return "fake" }
func (f *fakeSampler) Sample(context.Context) (Point, error) {
	if f.n.Add(1)%2 == 0 {
		return Point{}, errors.New("boom")
	}
	return Point{T: time.Now()}, nil
}

func TestCollect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Millisecond)
	defer cancel()
	var errs atomic.Int32
	ch := Collect(ctx, &fakeSampler{}, 20*time.Millisecond, func(error) { errs.Add(1) })
	got := 0
	start := time.Now()
	for range ch {
		got++
		if got == 1 && time.Since(start) > 15*time.Millisecond {
			t.Error("first sample should be immediate")
		}
	}
	if got < 2 || errs.Load() < 1 {
		t.Fatalf("points=%d errs=%d", got, errs.Load())
	}
	p, _ := NewNone().Sample(context.Background())
	if NewNone().Kind() != "none" || p.RSSBytes != nil || p.T.IsZero() {
		t.Fatal("none sampler")
	}
}
