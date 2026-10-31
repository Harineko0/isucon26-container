package isureal

import "context"

// Search and the ranked feed share a budget on each app server. At peak these
// two reads can crowd short queries out of the pool and contend on the 2-vCPU
// database. Queue before checkout, and return the slot as soon as rows are read.
func newHeavyReadSlots(region string) chan struct{} {
	limit := 4
	if region != "asia" {
		// The slot also covers the ~100 ms round trip to the database.
		limit = 8
	}
	return make(chan struct{}, limit)
}

func (h *HandlerContext) selectHeavy(ctx context.Context, dest any, query string, args ...any) error {
	if slots := h.heavyReads; slots != nil {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.DB.SelectContext(ctx, dest, query, args...)
}
