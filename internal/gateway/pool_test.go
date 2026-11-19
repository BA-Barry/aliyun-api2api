package gateway

import (
	"sync"
	"testing"
)

func TestPoolConcurrentAcquireRelease(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1", "key-a", "key-b", "key-c")
	p := NewPool(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire(nil)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			lease.Release()
			lease.Release()
		}()
	}
	wg.Wait()
	for _, status := range p.Status() {
		if status.Inflight != 0 {
			t.Errorf("%s inflight = %d", status.ID, status.Inflight)
		}
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1", "key-a", "key-b")
	cfg.Strategy = "round_robin"
	cfg.Upstreams[0].Weight = 2
	cfg.Upstreams[1].Weight = 1
	p := NewPool(cfg)
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		lease, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[lease.Account.ID]++
		lease.Release()
	}
	if counts["key-a"] != 4 || counts["key-b"] != 2 {
		t.Fatalf("weighted counts = %#v", counts)
	}
}
