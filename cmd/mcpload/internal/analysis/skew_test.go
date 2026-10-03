package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func TestVersionSkewVerdict(t *testing.T) {
	neg := map[string]map[string]int64{"2026-07-28": {"skew-new": 412}, "2025-11-25": {"skew-old": 130}}
	reps := map[string]int64{"skew-new": 600, "skew-old": 400}
	cases := []struct {
		name   string
		in     SkewInput
		status string
		want   []string
	}{
		{"no requests", SkewInput{}, report.StatusSkipped, []string{"recorded no requests"}},
		{"one build, no failures", SkewInput{Requests: 1000, Replicas: map[string]int64{"a": 500, "b": 500},
			Negotiated: map[string]map[string]int64{"2026-07-28": {"a": 60, "b": 40}}},
			report.StatusPass, []string{"All 1000 requests succeeded", "2026-07-28 via a (60)", "a 50%, b 50%"}},
		{"only one replica answered", SkewInput{Requests: 10, Replicas: map[string]int64{"a": 10}},
			report.StatusPass, []string{"Only one replica answered"}},
		{"all fast", SkewInput{Requests: 3000, Replicas: reps, Negotiated: neg,
			Failures:        map[string]map[string]int64{SkewFast: {"unsupported_protocol": 340, "http_400": 22, "unknown_tool": 5}},
			FailureMedianMs: map[string]float64{SkewFast: 2}},
			report.StatusWarn, []string{"367 of 3000 requests (12.2%) failed", "367 failed fast with typed errors (good",
				"340 × Unsupported protocol version (-32022), 22 × HTTP 400, 5 × unknown tool",
				"2026-07-28 via skew-new (412), 2025-11-25 via skew-old (130)", "skew-new 60%, skew-old 40%"}},
		{"slow typed errors warn", SkewInput{Requests: 100,
			Failures:        map[string]map[string]int64{SkewSlow: {"http_503": 4}},
			FailureMedianMs: map[string]float64{SkewSlow: 4200}},
			report.StatusWarn, []string{"4 failed slowly or without a typed error (median 4.2 s): 4 × HTTP 503"}},
		{"any hang fails", SkewInput{Requests: 3000, Replicas: map[string]int64{"skew-new": 2978, SkewNoAnswer: 22},
			Failures:        map[string]map[string]int64{SkewFast: {"unsupported_protocol": 340}, SkewHang: {"timeout": 22}},
			FailureMedianMs: map[string]float64{SkewFast: 2, SkewHang: 30000}},
			report.StatusFail, []string{"362 of 3000 requests (12.1%)", "340 failed fast", "22 hung for a median 30 s (bad: clients will hang during a rolling deploy): 22 × no answer before the client timeout",
				"skew-new 99%, no answer 1%"}},
		{"zero counts are ignored", SkewInput{Requests: 50, Failures: map[string]map[string]int64{SkewHang: {"timeout": 0}}},
			report.StatusPass, []string{"All 50 requests succeeded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := VersionSkewVerdict(tc.in)
			if v.ID != report.VerdictVersionSkew || v.Status != tc.status {
				t.Fatalf("id %s status %s, want %s: %s", v.ID, v.Status, tc.status, v.Message)
			}
			for _, w := range tc.want {
				if !strings.Contains(v.Message, w) {
					t.Errorf("message %q lacks %q", v.Message, w)
				}
			}
		})
	}
}

func TestSkewReasonText(t *testing.T) {
	for in, want := range map[string]string{"http_400": "HTTP 400", "unsupported_protocol": "Unsupported protocol version (-32022)", "new_reason": "new_reason"} {
		if got := SkewReasonText(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
