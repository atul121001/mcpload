package k6run

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Options is the subset of `k6 inspect` output mcpload uses.
type Options struct {
	Scenarios  map[string]Scenario
	Thresholds []ThresholdDef // sorted by metric key, script order within a key
	Tags       map[string]string
	VUs        *int
	Duration   string
}

// ThresholdDef is one threshold expression on one metric key.
type ThresholdDef struct {
	Metric string // e.g. mcp_req_duration{tool:search}
	Expr   string // e.g. p(95)<800
}

// Scenario is the subset of a k6 scenario config used for run.load.
type Scenario struct {
	Executor        string   `json:"executor"`
	Exec            string   `json:"exec"`
	StartTime       string   `json:"startTime"`
	Duration        string   `json:"duration"`
	VUs             *int     `json:"vus"`
	PreAllocatedVUs *int     `json:"preAllocatedVUs"`
	MaxVUs          *int     `json:"maxVUs"`
	StartVUs        *int     `json:"startVUs"`
	Rate            *float64 `json:"rate"`
	StartRate       *float64 `json:"startRate"`
	TimeUnit        string   `json:"timeUnit"`
	Stages          []struct {
		Duration string  `json:"duration"`
		Target   float64 `json:"target"`
	} `json:"stages"`
}

type rawOptions struct {
	Scenarios  map[string]Scenario        `json:"scenarios"`
	Thresholds map[string]json.RawMessage `json:"thresholds"`
	Tags       map[string]string          `json:"tags"`
	VUs        *int                       `json:"vus"`
	Duration   *string                    `json:"duration"`
}

// ParseOptions parses `k6 inspect` JSON.
func ParseOptions(b []byte) (*Options, error) {
	var raw rawOptions
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse k6 inspect output: %w", err)
	}
	o := &Options{Scenarios: raw.Scenarios, Tags: raw.Tags, VUs: raw.VUs}
	if raw.Duration != nil {
		o.Duration = *raw.Duration
	}
	keys := make([]string, 0, len(raw.Thresholds))
	for k := range raw.Thresholds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		exprs, err := thresholdExprs(raw.Thresholds[k])
		if err != nil {
			return nil, fmt.Errorf("threshold %s: %w", k, err)
		}
		for _, e := range exprs {
			o.Thresholds = append(o.Thresholds, ThresholdDef{Metric: k, Expr: e})
		}
	}
	return o, nil
}

// thresholdExprs accepts ["p(95)<800", {"threshold":"p(99)<2000","abortOnFail":true}].
func thresholdExprs(raw json.RawMessage) ([]string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		// k6 also accepts a single string
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 == nil {
			return []string{s}, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		var s string
		if err := json.Unmarshal(it, &s); err == nil {
			out = append(out, s)
			continue
		}
		var obj struct {
			Threshold string `json:"threshold"`
		}
		if err := json.Unmarshal(it, &obj); err != nil {
			return nil, err
		}
		out = append(out, obj.Threshold)
	}
	return out, nil
}

// Load is the load shape during the constant-load phase (report run.load).
type Load struct {
	Executor         string
	VUs              *int
	MaxVUs           *int
	ArrivalRate      *float64
	ArrivalTimeUnitS *float64
}

// MainScenario picks the scenario that represents the constant-load phase:
// the one named "load" if present, else the non-idle scenario with the most
// VUs (ties broken by name).
func (o *Options) MainScenario() (string, *Scenario) {
	if o == nil || len(o.Scenarios) == 0 {
		return "", nil
	}
	if s, ok := o.Scenarios["load"]; ok {
		return "load", &s
	}
	names := make([]string, 0, len(o.Scenarios))
	for n := range o.Scenarios {
		names = append(names, n)
	}
	sort.Strings(names)
	best, bestN := "", -1
	for _, n := range names {
		s := o.Scenarios[n]
		if s.Exec == "idle" {
			continue
		}
		v := maxVUsOf(s)
		if v > bestN {
			best, bestN = n, v
		}
	}
	if best == "" {
		best = names[0]
	}
	s := o.Scenarios[best]
	return best, &s
}

func maxVUsOf(s Scenario) int {
	m := 0
	for _, p := range []*int{s.VUs, s.PreAllocatedVUs, s.MaxVUs, s.StartVUs} {
		if p != nil && *p > m {
			m = *p
		}
	}
	if s.Executor == "ramping-vus" {
		for _, st := range s.Stages {
			if int(st.Target) > m {
				m = int(st.Target)
			}
		}
	}
	return m
}

// LoadShape derives run.load. Without options (inspect failed) it falls back to
// the given executor name.
func (o *Options) LoadShape() Load {
	_, s := o.MainScenario()
	if s == nil {
		l := Load{Executor: "constant-vus"}
		if o != nil && o.VUs != nil {
			l.VUs = o.VUs
		} else {
			one := 1
			l.VUs = &one
		}
		return l
	}
	l := Load{Executor: s.Executor}
	switch s.Executor {
	case "constant-arrival-rate", "ramping-arrival-rate":
		l.VUs = s.PreAllocatedVUs
		l.MaxVUs = s.MaxVUs
		rate := s.Rate
		if s.Executor == "ramping-arrival-rate" {
			var m float64
			if s.StartRate != nil {
				m = *s.StartRate
			}
			for _, st := range s.Stages {
				if st.Target > m {
					m = st.Target
				}
			}
			rate = &m
		}
		l.ArrivalRate = rate
		tu := 1.0
		if s.TimeUnit != "" {
			if d, err := time.ParseDuration(s.TimeUnit); err == nil && d > 0 {
				tu = d.Seconds()
			}
		}
		l.ArrivalTimeUnitS = &tu
	case "ramping-vus":
		v := maxVUsOf(*s)
		l.MaxVUs = &v
		l.VUs = s.StartVUs
	default:
		l.VUs = s.VUs
		if l.VUs == nil {
			one := 1
			l.VUs = &one
		}
	}
	return l
}

// ScenarioName is options.tags.scenario_name when the script sets it.
func (o *Options) ScenarioName() string {
	if o == nil || o.Tags == nil {
		return ""
	}
	return o.Tags["scenario_name"]
}
