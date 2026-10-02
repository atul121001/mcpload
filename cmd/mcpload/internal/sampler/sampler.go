// Package sampler collects server-side resource samples (RSS, heap, open FDs,
// active sessions) from the system under test during a run.
package sampler

import (
	"context"
	"time"
)

// Point is one sample. A nil field means the sampler could not observe it.
type Point struct {
	T              time.Time
	RSSBytes       *float64
	HeapBytes      *float64
	OpenFDs        *float64
	ActiveSessions *float64
}

// Sampler takes one sample per call. Kind returns the schema's
// series.server.sampler value: "docker", "prometheus" or "none".
type Sampler interface {
	Kind() string
	Sample(ctx context.Context) (Point, error)
}

type none struct{}

// NewNone returns a sampler that observes nothing (points carry only T).
func NewNone() Sampler { return none{} }

func (none) Kind() string { return "none" }

func (none) Sample(context.Context) (Point, error) { return Point{T: time.Now()}, nil }

// Collect samples s immediately and then every interval until ctx is done,
// sending each successful Point on the returned channel, which is closed when
// ctx is done. Failed samples are reported to onErr (may be nil) and skipped.
// The channel is unbuffered-ish (buffer 16); the consumer should drain it.
func Collect(ctx context.Context, s Sampler, interval time.Duration, onErr func(error)) <-chan Point {
	out := make(chan Point, 16)
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(out)
		sample := func() bool {
			p, err := s.Sample(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return false
				}
				if onErr != nil {
					onErr(err)
				}
				return true
			}
			if p.T.IsZero() {
				p.T = time.Now()
			}
			select {
			case out <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if ctx.Err() != nil || !sample() {
			return
		}
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if !sample() {
					return
				}
			}
		}
	}()
	return out
}

func fp(v float64) *float64 { return &v }
