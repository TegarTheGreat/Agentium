package provider

import (
	"context"
	"sync"
)

// Fallback tries a chain of models: when the current one fails with a
// retryable error (rate limit, overload, outage), the next one serves the
// request, and it stays the active model until it fails in turn.
type Fallback struct {
	Chain  []Resolved
	OnSwap func(from, to Resolved, err error)

	mu  sync.Mutex
	cur int
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
		if err == nil || !Retryable(err) || streamed || ctx.Err() != nil {
			if err == nil && idx != start {
				f.mu.Lock()
				f.cur = idx
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
