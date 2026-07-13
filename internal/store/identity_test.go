package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIdentityNonceIsConsumedExactlyOnceUnderRace(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "nonce.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	const workers = 32
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accepted, err := s.ConsumeIdentityNonce("ed25519_test", "nonce_test", now.Add(time.Minute), now)
			results <- accepted
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	accepted := 0
	for result := range results {
		if result {
			accepted++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d, want 1", accepted)
	}
	if ok, err := s.ConsumeIdentityNonce("ed25519_test", "expired", now, now); err == nil || ok {
		t.Fatalf("expired accepted=%t err=%v", ok, err)
	}
}
