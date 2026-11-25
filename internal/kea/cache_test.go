package kea_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

func TestReadCacheAsksOncePerRequest(t *testing.T) {
	fake, c := setup(t)
	ctx := kea.WithReadCache(context.Background())
	for range 3 {
		config, err := c.ConfigGet(ctx)
		if err != nil || len(config) == 0 {
			t.Fatal(err)
		}
		// a caller changing its copy leaves the others alone
		config[0] = 'x'
	}
	if _, err := c.ConfigGet(ctx); err != nil {
		t.Fatalf("copy was shared: %v", err)
	}
	for range 2 {
		c.Status(ctx)
		c.SubnetGet(ctx, 1)
	}
	if fake.CallCount("config-get") != 1 || fake.CallCount("subnet4-get") != 1 || fake.CallCount("status-get") != 1 {
		t.Errorf("cached reads: %v", fake.Calls)
	}
	// writes are never answered from the cache
	for range 2 {
		c.ConfigWrite(ctx)
	}
	if fake.CallCount("config-write") != 2 {
		t.Errorf("config-write was cached: %v", fake.Calls)
	}
	// without a cache, and with it switched off, every read goes to Kea
	c.ConfigGet(context.Background())
	c.ConfigGet(kea.WithoutReadCache(ctx))
	if fake.CallCount("config-get") != 3 {
		t.Errorf("uncached reads: %v", fake.Calls)
	}
}

// A caller that asks while the same request runs waits for it instead of
// sending its own.
func TestReadCacheSharesARunningRequest(t *testing.T) {
	fake, c := setup(t)
	gate := make(chan struct{})
	fake.Hold = map[string]chan struct{}{"config-get": gate}
	ctx := kea.WithReadCache(context.Background())
	first := make(chan error)
	go func() {
		_, err := c.ConfigGet(ctx)
		first <- err
	}()
	for fake.CallCount("config-get") == 0 {
		runtime.Gosched()
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := c.ConfigGet(short); err == nil || fake.CallCount("config-get") != 1 {
		t.Fatalf("the second caller did not wait: %v", err)
	}
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

// A caller with a short deadline gives up alone; the answer still reaches the others.
func TestReadCacheOutlivesAShortDeadline(t *testing.T) {
	fake, c := setup(t)
	gate := make(chan struct{})
	fake.Hold = map[string]chan struct{}{"config-get": gate}
	ctx := kea.WithReadCache(context.Background())
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := c.ConfigGet(short); err == nil {
		t.Fatal("no timeout")
	}
	close(gate)
	if _, err := c.ConfigGet(ctx); err != nil {
		t.Fatalf("the timeout was kept: %v", err)
	}
	if n := fake.CallCount("config-get"); n != 1 {
		t.Errorf("%d requests", n)
	}
}
