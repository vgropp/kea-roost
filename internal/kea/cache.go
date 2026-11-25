package kea

import (
	"context"
	"sync"
)

// cachedCommands only read, so one answer serves every caller of a request.
var cachedCommands = map[string]bool{
	"config-get": true, "lease4-get-all": true, "lease6-get-all": true,
	"reservation-get-all": true, "reservation-get": true, "statistic-get-all": true,
	"list-commands": true, "version-get": true, "class-get": true,
	"subnet4-get": true, "subnet6-get": true, "status-get": true, "ha-heartbeat": true,
}

type cacheKey struct{}

// readCache holds the answers of one request.
type readCache struct {
	// ctx is the request's own: it ends the shared requests to Kea with it
	ctx     context.Context
	mu      sync.Mutex
	entries map[entryKey]*cacheEntry
}

// entryKey is a client and the request it sends.
type entryKey struct {
	client *Client
	body   string
}

type cacheEntry struct {
	done chan struct{}
	resp *Response
	err  error
}

// WithReadCache lets the read commands sent with ctx share their answers: each
// is asked once per server for as long as ctx lives. It is meant for a single
// page load, never for a change, which must read what it just wrote. The
// answers are shared, so callers must not modify them.
func WithReadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheKey{}, &readCache{ctx: ctx, entries: map[entryKey]*cacheEntry{}})
}

// WithoutReadCache makes every command sent with ctx ask Kea again.
func WithoutReadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheKey{}, (*readCache)(nil))
}

// cacheOf returns the cache of ctx if command may be answered from it.
func cacheOf(ctx context.Context, command string) *readCache {
	if !cachedCommands[command] {
		return nil
	}
	cache, _ := ctx.Value(cacheKey{}).(*readCache)
	return cache
}

// ask answers a read command from the cache, asking Kea at most once. The
// request itself runs under the context of the whole page, not the deadline of
// whoever asked first, so a caller with a short timeout cannot leave a timeout
// behind for the others.
func (cache *readCache) ask(ctx context.Context, c *Client, command string, body []byte) (*Response, error) {
	key := entryKey{c, string(body)}
	cache.mu.Lock()
	e, ok := cache.entries[key]
	if !ok {
		e = &cacheEntry{done: make(chan struct{})}
		cache.entries[key] = e
		go func() {
			defer close(e.done)
			e.resp, e.err = c.send(cache.ctx, command, body)
		}()
	}
	cache.mu.Unlock()
	select {
	case <-e.done:
	case <-ctx.Done():
		return nil, &UnreachableError{Err: ctx.Err()}
	}
	return e.resp, e.err
}
