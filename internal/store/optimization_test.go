package store

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func qualityRecord(id, model, caseByte string, score int64) QualityScoreRecord {
	return QualityScoreRecord{
		Source: "eval", ScoreID: id, ObservedAt: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
		Model: model, Metric: "success", Cohort: "release-1", CaseHash: strings.Repeat(caseByte, 64),
		ScorePPM: score, Direction: "higher_is_better",
	}
}

func TestQualityScoreImportIdempotencyConflictAndImmutability(t *testing.T) {
	s, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 7, 12, 13, 0, 0, 0, time.UTC)
	record := qualityRecord("score-1", "model-a", "a", 900_000)
	result, err := s.ImportQualityScores([]QualityScoreRecord{record}, now)
	if err != nil || result.Inserted != 1 || result.Replayed != 0 {
		t.Fatalf("first import = %+v, %v", result, err)
	}
	result, err = s.ImportQualityScores([]QualityScoreRecord{record}, now.Add(time.Hour))
	if err != nil || result.Inserted != 0 || result.Replayed != 1 {
		t.Fatalf("replay = %+v, %v", result, err)
	}
	changed := record
	changed.ScorePPM--
	if _, err := s.ImportQualityScores([]QualityScoreRecord{changed}, now); !errors.Is(err, ErrQualityConflict) {
		t.Fatalf("identity conflict err = %v", err)
	}
	caseConflict := record
	caseConflict.ScoreID = "score-2"
	if _, err := s.ImportQualityScores([]QualityScoreRecord{caseConflict}, now); !errors.Is(err, ErrQualityCaseConflict) {
		t.Fatalf("case conflict err = %v", err)
	}
	if _, err := s.db.Exec(`UPDATE external_quality_scores SET score_ppm=0`); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable UPDATE err = %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM external_quality_scores`); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable DELETE err = %v", err)
	}
}

func TestQualityScoreBatchIsAtomicAndRejectsDuplicates(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	record := qualityRecord("score-1", "model-a", "a", 1)
	if _, err := s.ImportQualityScores([]QualityScoreRecord{record, record}, now); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate batch err = %v", err)
	}
	bad := qualityRecord("score-2", "model-a", "b", 1)
	bad.ScorePPM = math.MaxInt64
	if _, err := s.ImportQualityScores([]QualityScoreRecord{record, bad}, now); err == nil {
		t.Fatal("invalid batch accepted")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM external_quality_scores`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial batch persisted: count=%d err=%v", count, err)
	}
}

func TestQualityScoreBatchRollsBackNewRowsOnImmutableConflict(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	existing := qualityRecord("score-existing", "model-a", "a", 100_000)
	if _, err := s.ImportQualityScores([]QualityScoreRecord{existing}, now); err != nil {
		t.Fatal(err)
	}
	changed := existing
	changed.ScorePPM = 200_000
	newRecord := qualityRecord("score-new", "model-a", "b", 300_000)
	if _, err := s.ImportQualityScores([]QualityScoreRecord{newRecord, changed}, now); !errors.Is(err, ErrQualityConflict) {
		t.Fatalf("conflicting batch err = %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM external_quality_scores`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("conflicting batch was not atomic: count=%d err=%v", count, err)
	}
}

func TestQualityScoreConcurrentExactReplay(t *testing.T) {
	s, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := qualityRecord("score-1", "model-a", "a", 500_000)
	now := time.Now().UTC()
	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	results := make(chan QualityImportResult, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.ImportQualityScores([]QualityScoreRecord{record}, now)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent import: %v", err)
		}
	}
	inserted, replayed := 0, 0
	for result := range results {
		inserted += result.Inserted
		replayed += result.Replayed
	}
	if inserted != 1 || replayed != workers-1 {
		t.Fatalf("inserted=%d replayed=%d", inserted, replayed)
	}
}

func TestQualitySummariesCoverageIsCandidateBounded(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var records []QualityScoreRecord
	for modelIndex, model := range []string{"model-a", "model-b"} {
		cases := 2
		if modelIndex == 1 {
			cases = 1
		}
		for i := 0; i < cases; i++ {
			records = append(records, qualityRecord(fmt.Sprintf("%s-%d", model, i), model, string(rune('a'+i)), int64(800_000+i*100_000)))
		}
	}
	if _, err := s.ImportQualityScores(records, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	summaries, err := s.QualitySummaries(from, from.Add(24*time.Hour), "eval", "success", "release-1", []string{"model-a", "model-b", "model-c"})
	if err != nil {
		t.Fatal(err)
	}
	if summaries["model-a"].Samples != 2 || summaries["model-a"].Coverage != 1 || summaries["model-a"].AverageScorePPM != 850_000 {
		t.Fatalf("model-a = %+v", summaries["model-a"])
	}
	if summaries["model-b"].Samples != 1 || summaries["model-b"].Coverage != .5 {
		t.Fatalf("model-b = %+v", summaries["model-b"])
	}
	if _, ok := summaries["model-c"]; ok {
		t.Fatal("unscored candidate unexpectedly returned")
	}
}

func TestOptimizationRowsIsBoundedAndContentFree(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := s.Insert(Request{Ts: from.Add(time.Duration(i) * time.Hour), Provider: "p", Model: "m", Agent: "a",
			IdentityDevice: "meter-a", CostCenter: "platform", IdentityConfidence: "authenticated",
			BodyHash: "secret-fingerprint", InTokens: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	sample, err := s.OptimizationRows(from, from.Add(24*time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Rows) != 2 || !sample.Truncated || sample.Rows[0].InTokens != 3 ||
		sample.Rows[0].Meter != "meter-a" || sample.Rows[0].Team != "platform" ||
		sample.Rows[0].IdentityConfidence != "authenticated" {
		t.Fatalf("sample = %+v", sample)
	}
	if _, err := s.OptimizationRows(from, from.Add(MaxOptimizationRange+time.Hour), 10); err == nil {
		t.Fatal("unbounded time range accepted")
	}
}
