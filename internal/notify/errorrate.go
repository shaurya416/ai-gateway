package notify

import (
	"sync"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
)

// rateBuckets is how many slices one window is measured in. The window
// therefore slides in steps of a tenth of its length, and a sample is
// forgotten between nine and ten tenths of a window after it was taken.
const rateBuckets = 10

// rateTracker measures each target's error rate over a sliding window, for
// one webhook, and reports the attempt that carries it up to the threshold.
type rateTracker struct {
	threshold   float64
	window      time.Duration
	minRequests int
	width       int64 // nanoseconds per bucket

	mu      sync.Mutex
	targets map[string]*targetRate
}

// targetRate is one target's window.
type targetRate struct {
	buckets [rateBuckets]rateBucket
	// high is whether the last attempt left the rate at or above the
	// threshold. The event fires on its false→true edge and nowhere else, so
	// an outage reports once, not once per failed request, and a recovery
	// re-arms it for the next one.
	high bool
}

type rateBucket struct {
	slot          int64 // which bucket-width interval since the epoch this counts
	total, errors int
}

// rateReading is the window's contents after an attempt.
type rateReading struct {
	total, errors int
}

func (r rateReading) rate() float64 {
	if r.total == 0 {
		return 0
	}
	return float64(r.errors) / float64(r.total)
}

func newRateTracker(cfg config.ErrorRateConfig) *rateTracker {
	window := cfg.WindowDuration()
	width := int64(window) / rateBuckets
	if width <= 0 {
		width = 1
	}
	return &rateTracker{
		threshold:   cfg.Threshold,
		window:      window,
		minRequests: cfg.MinimumRequests(),
		width:       width,
		targets:     make(map[string]*targetRate),
	}
}

// observe records one attempt against target at now. fire is true exactly
// when this attempt moves the target's window from below the threshold — or
// from too few attempts to judge — to at or above it.
func (t *rateTracker) observe(target string, failed bool, now time.Time) (rateReading, bool) {
	slot := now.UnixNano() / t.width

	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.targets[target]
	if w == nil {
		w = &targetRate{}
		t.targets[target] = w
	}
	b := &w.buckets[bucketIndex(slot)]
	if b.slot != slot {
		*b = rateBucket{slot: slot}
	}
	b.total++
	if failed {
		b.errors++
	}

	var reading rateReading
	for i := range w.buckets {
		if s := w.buckets[i].slot; s > slot-rateBuckets && s <= slot {
			reading.total += w.buckets[i].total
			reading.errors += w.buckets[i].errors
		}
	}
	high := reading.total >= t.minRequests && reading.rate() >= t.threshold
	fire := high && !w.high
	w.high = high
	return reading, fire
}

func bucketIndex(slot int64) int {
	i := slot % rateBuckets
	if i < 0 {
		i += rateBuckets
	}
	return int(i)
}
