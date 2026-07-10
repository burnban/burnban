package main

import "testing"

func TestRunBench(t *testing.T) {
	res, err := runBench(30, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.recorded != 30 {
		t.Fatalf("recorded = %d, want every benched request in the ledger", res.recorded)
	}
	if res.proxied.p50 <= 0 || res.direct.p50 <= 0 {
		t.Fatalf("degenerate latencies: %+v", res)
	}
	// The proxy does strictly more work than the direct path; the overhead
	// subtraction must never go negative by construction.
	o := res.overhead()
	if o.p50 < 0 || o.p99 < 0 {
		t.Fatalf("negative overhead: %+v", o)
	}
}
