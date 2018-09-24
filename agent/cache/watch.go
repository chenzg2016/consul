package cache

import (
	"context"
	"fmt"
	"time"
)

// UpdateEvent is a struct summarising an update to a cache entry
type UpdateEvent struct {
	// CorrelationID is used by the Notify API to allow correlation of updates
	// with specific requests. We could return the full request object and
	// cachetype for consumers to match against the calls they made but in
	// practice it's cleaner for them to choose the minimal necessary unique
	// identifier given the set of things they are watching. They might even
	// choose to assign random IDs for example.
	CorrelationID string
	Result        interface{}
	Meta          ResultMeta
	Err           error
}

// Notify registers a desire to be updated about changes to a cache result.
//
// It is a helper that abstracts code from perfroming their own "blocking" query
// logic against a cache key to watch for changes and to maintain the key in
// cache actively. It will continue to perform blocking Get requests until the
// context is canceled.
//
// The passed context must be cancelled or timeout in order to free resources
// and stop maintaining the value in cache. Typically request-scoped resources
// do this but if a long-lived context like context.Background is used, then the
// caller must arrange for it to be cancelled when the watch is no longer
// needed.
//
// The passed chan may be buffered or unbuffered, if the caller doesn't consume
// fast enough it will block the notification loop. When the chan is later
// drained, watching resumes correctly. If the pause is longer than the
// cachetype's TTL, the result might be removed from the local cache. Even in
// this case though when the chan is drained again, the new Get will re-fetch
// the entry from servers and resume notification behaviour transparently.
//
// The chan is passed in to allow multiple cached results to be watched by a
// single consumer without juggling extra goroutines per watch. The
// correlationID is opaque and will be returned in all UpdateEvents generated by
// result of watching the specified request so the caller can set this to any
// value that allows them to dissambiguate between events in the returned chan
// when sharing a chan between multiple cache entries. If the chan is closed,
// the notify loop will terminate.
func (c *Cache) Notify(ctx context.Context, t string, r Request,
	correlationID string, ch chan<- UpdateEvent) error {

	// Get the type that we're fetching
	c.typesLock.RLock()
	tEntry, ok := c.types[t]
	c.typesLock.RUnlock()
	if !ok {
		return fmt.Errorf("unknown type in cache: %s", t)
	}
	if !tEntry.Type.SupportsBlocking() {
		return fmt.Errorf("watch requires the type to support blocking")
	}

	// Always start at 0 index to deliver the inital (possibly currently cached
	// value).
	index := uint64(0)

	go func() {
		var failures uint

		for {
			// Check context hasn't been cancelled
			if ctx.Err() != nil {
				return
			}

			// Blocking request
			res, meta, err := c.getWithIndex(t, r, index)

			// Check context hasn't been cancelled
			if ctx.Err() != nil {
				return
			}

			// Check the index of the value returned in the cache entry to be sure it
			// changed
			if index < meta.Index {
				u := UpdateEvent{correlationID, res, meta, err}
				select {
				case ch <- u:
				case <-ctx.Done():
					return
				}

				// Update index for next request
				index = meta.Index
			}

			// Handle errors with backoff. Badly behaved blocking calls that returned
			// a zero index are considered as failures since we need to not get stuck
			// in a busy loop.
			if err == nil && meta.Index > 0 {
				failures = 0
			} else {
				failures++
			}
			if wait := backOffWait(failures); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
			// Sanity check we always request blocking on second pass
			if index < 1 {
				index = 1
			}
		}
	}()

	return nil
}
