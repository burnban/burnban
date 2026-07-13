package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/burnban/burnban/internal/downshift"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func cmdDownshift(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: burnban downshift <validate|simulate|apply|show|disable> [flags]")
	}
	switch args[0] {
	case "validate":
		return cmdDownshiftValidate(args[1:])
	case "simulate":
		return cmdDownshiftSimulate(args[1:])
	case "apply":
		return cmdDownshiftApply(args[1:])
	case "show":
		return cmdDownshiftShow(args[1:])
	case "disable":
		return cmdDownshiftDisable(args[1:])
	default:
		return fmt.Errorf("unknown downshift command %q (use validate, simulate, apply, show, or disable)", args[0])
	}
}

func cmdDownshiftValidate(args []string) error {
	fs := flag.NewFlagSet("downshift validate", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: burnban downshift validate CONFIG.json")
	}
	compiled, err := readDownshiftFile(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("valid %s revision %d: %d exact mappings, mode %s, digest %s\n",
		compiled.Config.APIVersion, compiled.Config.Revision, len(compiled.Config.Rules), compiled.Config.Mode, compiled.Digest)
	return nil
}

func cmdDownshiftSimulate(args []string) error {
	fs := flag.NewFlagSet("downshift simulate", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	since := fs.String("since", "30d", `historical window: "today", "24h", "30d", or any Go duration`)
	format := fs.String("format", "text", "text or json")
	maxRows := fs.Int("max-rows", 250000, "maximum historical rows to replay")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: burnban downshift simulate [--db PATH] [--since 30d] CONFIG.json")
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("bad --format %q: use text or json", *format)
	}
	compiled, err := readDownshiftFile(fs.Arg(0))
	if err != nil {
		return err
	}
	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	report, id, err := runDownshiftSimulation(s, compiled, from, time.Now().UTC(), *maxRows)
	if err != nil {
		return err
	}
	return printDownshiftSimulation(report, id, *format)
}

func cmdDownshiftApply(args []string) error {
	fs := flag.NewFlagSet("downshift apply", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	since := fs.String("since", "30d", "historical review window")
	maxRows := fs.Int("max-rows", 250000, "maximum historical rows to replay")
	force := fs.Bool("force", false, "activate without a material historical sample (requires --force-reason)")
	forceReason := fs.String("force-reason", "", "durable operator explanation for forced activation")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: burnban downshift apply [--db PATH] [--since 30d] [--force --force-reason TEXT] CONFIG.json")
	}
	if *force != (*forceReason != "") {
		return fmt.Errorf("--force and --force-reason must be provided together")
	}
	compiled, err := readDownshiftFile(fs.Arg(0))
	if err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	var simulationID int64
	if !*force {
		from, _, err := parseSince(*since)
		if err != nil {
			return err
		}
		report, id, err := runDownshiftSimulation(s, compiled, from, time.Now().UTC(), *maxRows)
		if err != nil {
			return err
		}
		simulationID = id
		if err := printDownshiftSimulation(report, id, "text"); err != nil {
			return err
		}
	}
	if err := s.ApplyDownshiftDocument(store.DownshiftDocumentRecord{
		AppliedAt: time.Now().UTC(), APIVersion: compiled.Config.APIVersion,
		Revision: compiled.Config.Revision, Digest: compiled.Digest, Mode: string(compiled.Config.Mode),
		DocumentJSON: string(compiled.Canonical), SimulationID: simulationID,
		Forced: *force, ForceReason: *forceReason,
	}); err != nil {
		return err
	}
	fmt.Printf("activated downshift config revision %d in %s mode (digest %s)\n",
		compiled.Config.Revision, compiled.Config.Mode, compiled.Digest)
	if *force {
		fmt.Println("forced activation and its operator reason were appended to the immutable downshift audit")
	}
	return nil
}

func cmdDownshiftShow(args []string) error {
	fs := flag.NewFlagSet("downshift show", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	record, err := s.ActiveDownshiftDocument()
	if err != nil {
		return err
	}
	if record == nil {
		fmt.Println("no active downshift config; Burnban will not rewrite models or routes")
		return nil
	}
	compiled, err := downshift.Parse([]byte(record.DocumentJSON))
	if err != nil || compiled.Digest != record.Digest {
		return fmt.Errorf("active downshift config is invalid or metadata-mismatched")
	}
	fmt.Printf("active revision %d · mode %s · applied %s · digest %s\n",
		record.Revision, record.Mode, record.AppliedAt.UTC().Format(time.RFC3339), record.Digest)
	_, err = os.Stdout.Write(append(compiled.Canonical, '\n'))
	return err
}

func cmdDownshiftDisable(args []string) error {
	fs := flag.NewFlagSet("downshift disable", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	reason := fs.String("reason", "", "durable reason for disabling routing (10-500 bytes)")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *reason == "" {
		return fmt.Errorf("--reason is required so the routing change is auditable")
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.DisableDownshift(time.Now().UTC(), *reason); err != nil {
		return err
	}
	fmt.Println("downshift routing disabled; the reason was appended to the immutable audit")
	return nil
}

var errDownshiftSimulationLimit = errors.New("downshift simulation row limit reached")

func runDownshiftSimulation(s *store.Store, compiled *downshift.Compiled, from, through time.Time, maxRows int) (downshift.SimulationReport, int64, error) {
	if maxRows < 1 || maxRows > 1_000_000 {
		return downshift.SimulationReport{}, 0, fmt.Errorf("--max-rows must be between 1 and 1000000")
	}
	prices, err := pricing.Load()
	if err != nil {
		return downshift.SimulationReport{}, 0, err
	}
	simulator, err := downshift.NewSimulation(compiled, prices, from, through)
	if err != nil {
		return downshift.SimulationReport{}, 0, err
	}
	rows := 0
	err = s.StreamExport(from, func(row store.Request) error {
		if !row.Ts.Before(through) {
			return nil
		}
		if rows == maxRows {
			return errDownshiftSimulationLimit
		}
		rows++
		simulator.Add(row)
		return nil
	})
	if errors.Is(err, errDownshiftSimulationLimit) {
		return downshift.SimulationReport{}, 0, fmt.Errorf("more than %d rows match; narrow --since or raise --max-rows", maxRows)
	}
	if err != nil {
		return downshift.SimulationReport{}, 0, err
	}
	report := simulator.Finish()
	reportJSON, err := jsonMarshalBounded(report)
	if err != nil {
		return downshift.SimulationReport{}, 0, err
	}
	id, err := s.InsertDownshiftSimulation(store.DownshiftSimulationRecord{
		CreatedAt: through, ConfigDigest: compiled.Digest, Since: from, Through: through,
		TotalRequests: report.TotalRequests, MatchedRequests: report.MatchedRequests,
		EligibleRequests: report.EligibleRequests, IndeterminateRequests: report.IndeterminateRequests,
		SourceCostUSD: report.SourceCostUSD, TargetCostUSD: report.TargetCostUSD, ReportJSON: string(reportJSON),
	})
	return report, id, err
}

func jsonMarshalBounded(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 1<<20 {
		return nil, fmt.Errorf("simulation report exceeds 1 MiB")
	}
	return encoded, nil
}

func printDownshiftSimulation(report downshift.SimulationReport, id int64, format string) error {
	if format == "json" {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			SimulationID int64 `json:"simulation_id"`
			downshift.SimulationReport
		}{id, report})
	}
	fmt.Printf("downshift historical review #%d · config revision %d · %s to %s\n", id, report.ConfigRevision,
		report.Since.Format(time.RFC3339), report.Through.Format(time.RFC3339))
	fmt.Printf("requests %d · matched %d · eligible %d · incompatible %d · indeterminate %d\n",
		report.TotalRequests, report.MatchedRequests, report.EligibleRequests, report.IneligibleRequests, report.IndeterminateRequests)
	fmt.Printf("eligible impact $%.4f source → $%.4f target · savings $%.4f (%.2f%%)\n",
		report.SourceCostUSD, report.TargetCostUSD, report.SavingsUSD, report.SavingsPct)
	for _, note := range report.Notes {
		fmt.Printf("note: %s\n", note)
	}
	return nil
}

func readDownshiftFile(path string) (*downshift.Compiled, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("downshift config must be a regular file (symlinks and devices are rejected)")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("downshift config changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("downshift config exceeds 1 MiB")
	}
	afterRead, statErr := file.Stat()
	pathAfter, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !afterRead.Mode().IsRegular() || !pathAfter.Mode().IsRegular() ||
		!os.SameFile(opened, afterRead) || !os.SameFile(afterRead, pathAfter) ||
		afterRead.Size() != opened.Size() || !afterRead.ModTime().Equal(opened.ModTime()) || int64(len(raw)) != afterRead.Size() {
		return nil, fmt.Errorf("downshift config changed while reading")
	}
	return downshift.Parse(raw)
}
