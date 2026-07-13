package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/store"
)

func cmdPolicy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: burnban policy <validate|apply|show|simulate|coverage|templates|reset|takeover|events> [flags]")
	}
	switch args[0] {
	case "validate":
		return cmdPolicyValidate(args[1:])
	case "apply":
		return cmdPolicyApply(args[1:])
	case "show":
		return cmdPolicyShow(args[1:])
	case "simulate":
		return cmdPolicySimulate(args[1:])
	case "coverage":
		return cmdPolicyCoverage(args[1:])
	case "templates":
		return cmdPolicyTemplates(args[1:])
	case "reset":
		return cmdPolicyLineageChange(args[1:], false)
	case "takeover":
		return cmdPolicyLineageChange(args[1:], true)
	case "events":
		return cmdPolicyEvents(args[1:])
	default:
		return fmt.Errorf("unknown policy command %q", args[0])
	}
}

func cmdPolicyLineageChange(args []string, takeover bool) error {
	name := "reset"
	if takeover {
		name = "takeover"
	}
	fs := flag.NewFlagSet("policy "+name, flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	digest := fs.String("confirm-digest", "", "required exact active policy digest")
	source := fs.String("confirm-source", "", "required exact active policy source")
	reason := fs.String("reason", "", "required audit reason")
	actor := fs.String("actor", "local-operator", "audit actor")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *digest == "" || *source == "" || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("policy %s requires --confirm-digest, --confirm-source, and --reason", name)
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.ResetPolicyLineage(store.PolicyLineageReset{
		Actor: *actor, Reason: *reason, ExpectedDigest: *digest, ExpectedSource: *source, Takeover: takeover,
	}); err != nil {
		return err
	}
	fmt.Printf("policy lineage %s recorded for source %s digest %s\n", name, *source, *digest)
	return nil
}

func cmdPolicyEvents(args []string) error {
	fs := flag.NewFlagSet("policy events", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	limit := fs.Int("limit", 100, "maximum events")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	events, err := s.PolicyLineageEvents(*limit)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(events)
}

func cmdPolicyValidate(args []string) error {
	fs := flag.NewFlagSet("policy validate", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: burnban policy validate POLICY.json")
	}
	compiled, err := readPolicyFile(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("valid %s %s revision %d: %d rules, digest %s\n", compiled.Document.APIVersion,
		compiled.Document.Metadata.Name, compiled.Document.Metadata.Revision,
		len(compiled.Document.Rules), compiled.Digest)
	return nil
}

func cmdPolicyApply(args []string) error {
	fs := flag.NewFlagSet("policy apply", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: burnban policy apply [--db PATH] POLICY.json")
	}
	compiled, err := readPolicyFile(fs.Arg(0))
	if err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	canonical, err := json.Marshal(compiled.Document)
	if err != nil {
		return err
	}
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		AppliedAt: time.Now().UTC(), APIVersion: compiled.Document.APIVersion,
		Name: compiled.Document.Metadata.Name, Namespace: compiled.Document.Metadata.Namespace,
		Revision: compiled.Document.Metadata.Revision,
		Digest:   compiled.Digest, DocumentJSON: string(canonical),
	}); err != nil {
		return err
	}
	fmt.Printf("applied policy %s revision %d (%d rules, digest %s)\n",
		compiled.Document.Metadata.Name, compiled.Document.Metadata.Revision,
		len(compiled.Document.Rules), compiled.Digest)
	return nil
}

func cmdPolicyShow(args []string) error {
	fs := flag.NewFlagSet("policy show", flag.ExitOnError)
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
	record, err := s.ActivePolicyDocument()
	if err != nil {
		return err
	}
	if record == nil {
		fmt.Println("no active v2 policy; existing caps, fuses, and burn bans still apply")
		return nil
	}
	compiled, err := policy.Parse([]byte(record.DocumentJSON))
	if err != nil {
		return fmt.Errorf("active policy is invalid: %w", err)
	}
	pretty, err := json.MarshalIndent(compiled.Document, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", pretty)
	return nil
}

func cmdPolicySimulate(args []string) error {
	fs := flag.NewFlagSet("policy simulate", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	format := fs.String("format", "text", "text or json")
	maxRows := fs.Int("max-rows", 250000, "maximum historical rows to replay")
	fs.Parse(args)
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: burnban policy simulate [--db PATH] [--since 7d] [POLICY.json]")
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("bad --format %q: use text or json", *format)
	}
	if *maxRows < 1 || *maxRows > 1_000_000 {
		return fmt.Errorf("--max-rows must be between 1 and 1000000")
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
	var compiled *policy.Compiled
	if fs.NArg() == 1 {
		compiled, err = readPolicyFile(fs.Arg(0))
	} else {
		var record *store.PolicyDocumentRecord
		record, err = s.ActivePolicyDocument()
		if err == nil && record == nil {
			return fmt.Errorf("no active policy; pass a candidate POLICY.json")
		}
		if err == nil {
			compiled, err = policy.Parse([]byte(record.DocumentJSON))
		}
	}
	if err != nil {
		return err
	}
	tooMany := errors.New("simulation row limit reached")
	samples := make([]policy.HistoricalSample, 0, min(*maxRows, 4096))
	err = s.StreamExport(from, func(row store.Request) error {
		if len(samples) == *maxRows {
			return tooMany
		}
		context, known := historicalPolicyContext(row)
		samples = append(samples, policy.HistoricalSample{
			Ts: row.Ts, End: row.Ts.Add(time.Duration(max(row.LatencyMs, 0)) * time.Millisecond),
			Context: context, AdmissionKnown: known,
		})
		return nil
	})
	if errors.Is(err, tooMany) {
		return fmt.Errorf("more than %d request rows match; narrow --since or raise --max-rows", *maxRows)
	}
	if err != nil {
		return err
	}
	report := policy.Simulate(compiled, samples)
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Printf("policy %s revision %d — historical simulation (%s confidence)\n",
		report.PolicyName, report.PolicyRevision, report.Confidence)
	fmt.Printf("historical result: would block %d of %d calls; affected agents: %d",
		report.WouldBlock, report.Requests, report.Affected.Agents.Distinct)
	if report.Affected.Agents.Limited {
		fmt.Print(" (bounded list; more were present)")
	}
	fmt.Println()
	fmt.Printf("would allow: %d  warnings: %d  shadow-only impacts: %d  indeterminate: %d\n",
		report.WouldAllow, report.WouldWarn, report.WouldObserve, report.Indeterminate)
	if len(report.Reasons) != 0 {
		encoded, _ := json.Marshal(report.Reasons)
		fmt.Printf("denial reasons: %s\n", encoded)
	}
	affected, _ := json.Marshal(report.Affected)
	fmt.Printf("affected breakdown: %s\n", affected)
	fmt.Printf("precedence: %s\n", report.Precedence)
	if len(report.RuleInteractions) != 0 {
		encoded, _ := json.Marshal(report.RuleInteractions)
		fmt.Printf("rule interactions: %s\n", encoded)
	}
	if report.RuleInteractionsLimited {
		fmt.Println("note: rule interaction explanations were bounded; more combinations were present")
	}
	for _, note := range report.Notes {
		fmt.Printf("note: %s\n", note)
	}
	return nil
}

func historicalPolicyContext(row store.Request) (policy.Context, bool) {
	if row.Policy != nil && row.Policy.ContextJSON != "" {
		var context policy.Context
		if json.Unmarshal([]byte(row.Policy.ContextJSON), &context) == nil {
			return context, true
		}
	}
	input := saturatingHistoricalTokenSum(row.InTokens, row.CacheReadTokens, row.CacheWriteTokens, row.CacheWrite1hTokens)
	return policy.Context{
		Agent: row.Agent, Provider: row.Provider, Model: row.Model, Route: row.Route,
		Tier: row.ServiceTier, Geo: row.InferenceGeo, EstimatedInput: input,
		EstimatedCostUSD:   row.CostUSD,
		CostKnown:          row.PricingState == store.PricingPriced && !row.FeeUnpriced,
		IdentityConfidence: "unverified",
	}, false
}

func saturatingHistoricalTokenSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func cmdPolicyTemplates(args []string) error {
	fs := flag.NewFlagSet("policy templates", flag.ExitOnError)
	list := fs.Bool("list", false, "list template names")
	fs.Parse(args)
	if *list {
		if fs.NArg() != 0 {
			return fmt.Errorf("--list does not accept a template name")
		}
		fmt.Println(strings.Join(policyTemplateNames(), "\n"))
		return nil
	}
	name := "starter"
	if fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("usage: burnban policy templates [--list|NAME]")
	}
	document, ok := policyTemplate(name)
	if !ok {
		return fmt.Errorf("unknown template %q (list templates with --list)", name)
	}
	pretty, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", pretty)
	return nil
}

func policyTemplateNames() []string {
	return []string{
		"starter",
		"individual-coding-agent",
		"ci-review-bot",
		"autonomous-research-agent",
		"production-customer-app",
		"local-private-model",
		"unknown-price-sandbox",
		"hierarchical",
		"shadow",
	}
}

func policyTemplate(name string) (policy.Document, bool) {
	cost := 0.50
	base := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: name, Namespace: name, Revision: 1}, Mode: policy.ModeEnforce,
	}
	switch name {
	case "starter":
		base.Rules = []policy.Rule{{
			ID: "global-guardrails", Limits: policy.Limits{
				Requests: []policy.WindowLimit{{ID: "rpm", Max: 60, Window: "1m", WindowType: "rolling"}},
				Tokens: []policy.WindowLimit{
					{ID: "input-hour", Kind: "input", Max: 750_000, Window: "1h", WindowType: "fixed"},
					{ID: "output-hour", Kind: "output", Max: 250_000, Window: "1h", WindowType: "fixed"},
					{ID: "total-hour", Kind: "total", Max: 1_000_000, Window: "1h", WindowType: "fixed"},
				},
				Dollars:     []policy.DollarLimit{{ID: "daily-spend", MaxMicroUSD: 10_000_000, Window: "24h", WindowType: "fixed"}},
				Concurrency: 4, MaxEstimatedCallCostUSD: &cost, RequireOutputBound: true,
			},
		}}
	case "individual-coding-agent":
		base.Rules = []policy.Rule{{
			ID: "interactive-workstation", Limits: policy.Limits{
				Requests: []policy.WindowLimit{
					{ID: "rpm", Max: 60, Window: "1m", WindowType: "rolling"},
					{ID: "daily-requests", Max: 2_000, Window: "24h", WindowType: "fixed"},
				},
				Tokens:      []policy.WindowLimit{{ID: "tokens-hour", Max: 1_000_000, Window: "1h", WindowType: "rolling"}},
				Concurrency: 4, MaxEstimatedCallCostUSD: &cost, RequireOutputBound: true,
			},
		}}
	case "ci-review-bot":
		ciCost := 0.25
		base.Rules = []policy.Rule{{
			ID: "bounded-ci-review", Match: policy.Match{
				Tier: policy.AccessList{Deny: []string{"priority", "batch-premium"}},
			}, Limits: policy.Limits{
				Requests: []policy.WindowLimit{
					{ID: "rpm", Max: 30, Window: "1m", WindowType: "rolling"},
					{ID: "run-hour", Max: 300, Window: "1h", WindowType: "rolling"},
				},
				Tokens:      []policy.WindowLimit{{ID: "tokens-hour", Max: 500_000, Window: "1h", WindowType: "rolling"}},
				Concurrency: 4, MaxEstimatedCallCostUSD: &ciCost, RequireOutputBound: true,
			},
		}}
	case "autonomous-research-agent":
		researchCost := 0.20
		base.Rules = []policy.Rule{{
			ID: "autonomy-breaker", Match: policy.Match{
				Model: policy.AccessList{Deny: []string{"*-preview", "*experimental*"}},
			}, Limits: policy.Limits{
				Requests: []policy.WindowLimit{
					{ID: "rpm", Max: 20, Window: "1m", WindowType: "rolling"},
					{ID: "daily-requests", Max: 1_000, Window: "24h", WindowType: "fixed"},
				},
				Tokens:      []policy.WindowLimit{{ID: "tokens-hour", Max: 400_000, Window: "1h", WindowType: "rolling"}},
				Concurrency: 2, MaxEstimatedCallCostUSD: &researchCost, RequireOutputBound: true,
			},
		}}
	case "production-customer-app":
		productionCost := 0.10
		// Production starts in warn mode so operators can replay and inspect
		// impact before advancing the revision to enforce.
		base.Mode = policy.ModeWarn
		base.Rules = []policy.Rule{{
			ID: "production-stage", Scope: policy.Scope{Environment: []string{"production"}}, Match: policy.Match{
				Provider: policy.AccessList{Allow: []string{"anthropic", "openai", "gemini", "xai"}},
				Model:    policy.AccessList{Deny: []string{"*-preview", "*experimental*"}},
			}, Limits: policy.Limits{
				Requests:    []policy.WindowLimit{{ID: "rpm", Max: 300, Window: "1m", WindowType: "rolling"}},
				Tokens:      []policy.WindowLimit{{ID: "tokens-minute", Kind: "total", Max: 1_000_000, Window: "1m", WindowType: "rolling"}},
				Dollars:     []policy.DollarLimit{{ID: "hourly-spend", MaxMicroUSD: 50_000_000, Window: "1h", WindowType: "rolling"}},
				Concurrency: 32, MaxEstimatedCallCostUSD: &productionCost, RequireOutputBound: true,
			},
		}}
	case "local-private-model":
		base.Rules = []policy.Rule{{
			ID: "local-only", Match: policy.Match{
				Provider: policy.AccessList{Allow: []string{"local", "ollama", "vllm", "custom"}},
			}, Limits: policy.Limits{
				Requests:    []policy.WindowLimit{{ID: "rpm", Max: 120, Window: "1m", WindowType: "rolling"}},
				Tokens:      []policy.WindowLimit{{ID: "tokens-minute", Max: 2_000_000, Window: "1m", WindowType: "rolling"}},
				Concurrency: 8, RequireOutputBound: true,
			},
		}}
	case "unknown-price-sandbox":
		// This template intentionally has no dollar rule: request, token, and
		// concurrency limits remain enforceable when price is unknown or zero.
		base.Rules = []policy.Rule{{
			ID: "price-independent-sandbox", Limits: policy.Limits{
				Requests: []policy.WindowLimit{
					{ID: "rpm", Max: 30, Window: "1m", WindowType: "rolling"},
					{ID: "daily-requests", Max: 500, Window: "24h", WindowType: "fixed"},
				},
				Tokens:      []policy.WindowLimit{{ID: "tokens-hour", Max: 250_000, Window: "1h", WindowType: "rolling"}},
				Concurrency: 2, RequireOutputBound: true,
			},
		}}
	case "hierarchical":
		base.Rules = []policy.Rule{
			{ID: "organization-provider-boundary", Match: policy.Match{
				Provider: policy.AccessList{Allow: []string{"anthropic", "openai"}},
				Geo:      policy.AccessList{Allow: []string{"", "global", "us"}},
			}},
			{ID: "platform-codex", Scope: policy.Scope{Team: []string{"platform"}, Agent: []string{"codex*"}},
				Mode:  policy.ModeWarn,
				Match: policy.Match{Model: policy.AccessList{Deny: []string{"*-preview"}}},
				Limits: policy.Limits{Concurrency: 8, Requests: []policy.WindowLimit{
					{ID: "team-rpm", Max: 120, Window: "1m", WindowType: "rolling"},
				}}},
		}
	case "shadow":
		base.Mode = policy.ModeObserve
		base.Rules = []policy.Rule{{ID: "observe-expensive-calls", Limits: policy.Limits{
			MaxEstimatedCallCostUSD: &cost, RequireOutputBound: true,
		}}}
	default:
		return policy.Document{}, false
	}
	return base, true
}

func readPolicyFile(path string) (*policy.Compiled, error) {
	var reader io.Reader
	var closeFile func() error
	if path == "-" {
		reader = os.Stdin
		closeFile = func() error { return nil }
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		reader, closeFile = file, file.Close
	}
	defer closeFile()
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("policy document exceeds 1 MiB")
	}
	compiled, err := policy.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(path), err)
	}
	return compiled, nil
}
