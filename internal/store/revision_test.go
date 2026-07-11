package store

import (
	"path/filepath"
	"testing"
)

func TestRequestRevisionSeqlockIsOddDuringMutation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := s.RequestRevision()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.mutateRequests(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if revision := s.RequestRevision(); revision%2 != 1 {
		t.Fatalf("in-progress revision=%d, want odd", revision)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if revision := s.RequestRevision(); revision != start+2 || revision%2 != 0 {
		t.Fatalf("settled revision=%d, want stable %d", revision, start+2)
	}
}
