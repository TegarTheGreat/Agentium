package provider

import (
	"context"
	"sync"
	"time"
)

// Fallback tries a chain of models: when the current one fails with a
// retryable error (rate limit, overload, outage), the next one serves the
// request, and it stays the active model until it fails in turn — or
// until the first model has had a few minutes to recover: then it is
// tried again (an outage should not cost the rest of the session).
type Fallback struct {
	Chain  []Resolved
	OnSwap func(from, to Resolved, err error)
	// Recover is how long before the first model is tried again (0: 5 min).
	Recover time.Duration

	mu      sync.Mutex
	cur     int
	swapped time.Time
}

// Active returns the model currently serving requests.
func (f *Fallback) Active() Resolved {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Chain[f.cur]
}

// Stream implements Client. The request's Model is replaced by the model
// of the chain entry that serves it.
func (f *Fallback) Stream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	f.mu.Lock()
	wait := f.Recover
	if wait == 0 {
		wait = 5 * time.Minute
	}
	if f.cur != 0 && time.Since(f.swapped) > wait {
		f.cur = 0 // back to the first choice; it falls over again if still down
	}
	start := f.cur
	f.mu.Unlock()
	var resp Response
	var err error
	for i := 0; i < len(f.Chain); i++ {
		idx := (start + i) % len(f.Chain)
		r := f.Chain[idx]
		rq := req
		rq.Model = r.Model
		rq.Reasoning = r.Reasoning(req.Reasoning.Effort)
		streamed := false
		resp, err = r.Client.Stream(ctx, rq, func(s string) {
			streamed = true
			if onText != nil {
				onText(s)
			}
		})
		resp.Model = r.Model
		if err == nil || !Retryable(err) || streamed || ctx.Err() != nil {
			if err == nil && idx != start {
				f.mu.Lock()
				f.cur, f.swapped = idx, time.Now()
				f.mu.Unlock()
			}
			return resp, err
		}
		if i+1 < len(f.Chain) && f.OnSwap != nil {
			f.OnSwap(r, f.Chain[(idx+1)%len(f.Chain)], err)
		}
	}
	return resp, err
}
