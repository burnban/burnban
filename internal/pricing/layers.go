package pricing

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CostSource is the immutable provenance attached to a priced ledger row.
// Its order is also the resolution order: an exact provider amount wins over
// a negotiated contract, which wins over the public list. Unknown is never
// coerced into a plausible-looking zero-dollar price.
type CostSource string

const (
	SourceProviderFinal CostSource = "provider_final"
	SourceContract      CostSource = "contract"
	SourcePublicList    CostSource = "public_list"
	SourceUnknown       CostSource = "unknown"
	SourceUnmetered     CostSource = "unmetered"
)

// ContractPrice is a dated customer price override. Provider and model are
// exact selectors (dated provider model suffixes retain Lookup's conservative
// version matching). Region and service tier are optional additional scopes.
// More-specific matching contracts win; equal-specificity overlaps are
// rejected at load time so resolution cannot depend on file order.
type ContractPrice struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Region        string `json:"region,omitempty"`
	ServiceTier   string `json:"service_tier,omitempty"`
	EffectiveFrom string `json:"effective_from"`
	ValidThrough  string `json:"valid_through,omitempty"`
	Price         Price  `json:"price"`
}

// Resolution is one deterministic layer-selection result. Effective dates
// are copied into the request row rather than looked up at report time, so a
// later pricing-file change cannot rewrite historical provenance.
type Resolution struct {
	Price         Price
	FinalCostUSD  float64
	HasFinalCost  bool
	Source        CostSource
	SourceRef     string
	EffectiveFrom string
	ValidThrough  string
	CoversRegion  bool
	CoversTier    bool
}

// Resolve selects provider final cost, then a customer contract, then public
// list price. providerFinalPresent must be explicit because a genuine free
// provider-reported call can have a final amount of zero.
func (t *Table) Resolve(provider, model, region, serviceTier string, at time.Time, providerFinalUSD float64, providerFinalPresent bool) (Resolution, bool) {
	if providerFinalPresent {
		if validFinalCost(providerFinalUSD) {
			return Resolution{
				FinalCostUSD: providerFinalUSD, HasFinalCost: true,
				Source: SourceProviderFinal, SourceRef: strings.ToLower(strings.TrimSpace(provider)),
				EffectiveFrom: at.UTC().Format("2006-01-02"),
			}, true
		}
		// A malformed provider amount is evidence of unknown pricing, not
		// permission to silently substitute a weaker estimate.
		return Resolution{Source: SourceUnknown}, false
	}

	date := at.UTC().Format("2006-01-02")
	var winner *ContractPrice
	winnerSpecificity := -1
	for i := range t.Contracts {
		contract := &t.Contracts[i]
		if !contractMatches(*contract, provider, model, region, serviceTier, date) {
			continue
		}
		specificity := contractSpecificity(*contract)
		if specificity > winnerSpecificity {
			winner, winnerSpecificity = contract, specificity
		}
	}
	if winner != nil {
		return Resolution{
			Price: winner.Price, Source: SourceContract, SourceRef: winner.ID,
			EffectiveFrom: winner.EffectiveFrom, ValidThrough: winner.ValidThrough,
			CoversRegion: winner.Region != "", CoversTier: winner.ServiceTier != "",
		}, true
	}

	price, key, ok := t.lookupWithKey(model)
	if !ok || !dateInRange(date, price.EffectiveFrom, price.ValidThrough) {
		return Resolution{Source: SourceUnknown}, false
	}
	ref := price.Source
	if ref == "" {
		ref = key
	}
	return Resolution{
		Price: price, Source: SourcePublicList, SourceRef: ref,
		EffectiveFrom: price.EffectiveFrom, ValidThrough: price.ValidThrough,
	}, true
}

func validFinalCost(v float64) bool {
	// A single response above one billion dollars is not credible accounting
	// evidence and risks precision loss in downstream aggregates.
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1_000_000_000
}

func contractMatches(c ContractPrice, provider, model, region, tier, date string) bool {
	if !strings.EqualFold(c.Provider, strings.TrimSpace(provider)) || !modelMatches(c.Model, model) ||
		!dateInRange(date, c.EffectiveFrom, c.ValidThrough) {
		return false
	}
	if c.Region != "" && !strings.EqualFold(c.Region, strings.TrimSpace(region)) {
		return false
	}
	return c.ServiceTier == "" || strings.EqualFold(c.ServiceTier, strings.TrimSpace(tier))
}

func modelMatches(configured, observed string) bool {
	return observed == configured || strings.HasPrefix(observed, configured) && versionSuffix(observed[len(configured):])
}

func dateInRange(date, from, through string) bool {
	return (from == "" || date >= from) && (through == "" || date <= through)
}

func contractSpecificity(c ContractPrice) int {
	n := 2 // provider + model are always present
	if c.Region != "" {
		n++
	}
	if c.ServiceTier != "" {
		n++
	}
	return n
}

func validateContracts(contracts []ContractPrice) error {
	seen := map[string]struct{}{}
	for i, contract := range contracts {
		for field, value := range map[string]string{
			"id": contract.ID, "provider": contract.Provider, "model": contract.Model,
			"region": contract.Region, "service_tier": contract.ServiceTier,
		} {
			if err := validateSelector(field, value); err != nil {
				return fmt.Errorf("contract %d: %w", i, err)
			}
			if value != strings.TrimSpace(value) {
				return fmt.Errorf("contract %d %s must not have surrounding whitespace", i, field)
			}
		}
		contract.ID = strings.TrimSpace(contract.ID)
		contract.Provider = strings.TrimSpace(contract.Provider)
		contract.Model = strings.TrimSpace(contract.Model)
		contract.Region = strings.TrimSpace(contract.Region)
		contract.ServiceTier = strings.TrimSpace(contract.ServiceTier)
		if contract.ID == "" || len(contract.ID) > 200 || contract.Provider == "" || contract.Model == "" {
			return fmt.Errorf("contract %d requires a bounded id, provider, and model", i)
		}
		if _, duplicate := seen[contract.ID]; duplicate {
			return fmt.Errorf("duplicate contract id %q", contract.ID)
		}
		seen[contract.ID] = struct{}{}
		if contract.EffectiveFrom == "" {
			return fmt.Errorf("contract %q requires effective_from", contract.ID)
		}
		if err := validateDate("contract "+contract.ID+" effective_from", contract.EffectiveFrom); err != nil {
			return err
		}
		if contract.ValidThrough != "" {
			if err := validateDate("contract "+contract.ID+" valid_through", contract.ValidThrough); err != nil {
				return err
			}
			if contract.ValidThrough < contract.EffectiveFrom {
				return fmt.Errorf("contract %q valid_through must not precede effective_from", contract.ID)
			}
		}
		// Provenance belongs to the contract itself; nested public-source dates
		// would create two conflicting effective intervals.
		if contract.Price.Source != "" || contract.Price.EffectiveFrom != "" || contract.Price.ValidThrough != "" || contract.Price.VerifiedDate != "" {
			return fmt.Errorf("contract %q price must not contain public-list provenance", contract.ID)
		}
		if err := validateModels(map[string]Price{contract.Model: contract.Price}); err != nil {
			return fmt.Errorf("contract %q: %w", contract.ID, err)
		}
	}
	// Reject ambiguous equal-specificity overlaps. Sort first so the error is
	// deterministic regardless of JSON order.
	ordered := append([]ContractPrice(nil), contracts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for i := range ordered {
		for j := i + 1; j < len(ordered); j++ {
			if contractSpecificity(ordered[i]) != contractSpecificity(ordered[j]) ||
				!sameSelector(ordered[i], ordered[j]) || !rangesOverlap(ordered[i], ordered[j]) {
				continue
			}
			return fmt.Errorf("contracts %q and %q have ambiguous overlapping selectors", ordered[i].ID, ordered[j].ID)
		}
	}
	return nil
}

func validateSelector(field, value string) error {
	if len(value) > 200 || !utf8.ValidString(value) {
		return fmt.Errorf("%s exceeds 200 bytes", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return fmt.Errorf("%s contains unsafe Unicode", field)
		}
	}
	return nil
}

func sameSelector(a, b ContractPrice) bool {
	return strings.EqualFold(a.Provider, b.Provider) && modelSelectorsOverlap(a.Model, b.Model) &&
		strings.EqualFold(a.Region, b.Region) && strings.EqualFold(a.ServiceTier, b.ServiceTier)
}

// modelSelectorsOverlap mirrors modelMatches rather than comparing configured
// strings literally. A shorter version family such as "gpt-5" overlaps a
// more specific "gpt-5.4" selector because both can match
// "gpt-5.4-YYYYMMDD". Accepting both at equal dimensional specificity would
// make contract selection depend on JSON order.
func modelSelectorsOverlap(a, b string) bool {
	return modelMatches(a, b) || modelMatches(b, a)
}

func rangesOverlap(a, b ContractPrice) bool {
	aEnd, bEnd := a.ValidThrough, b.ValidThrough
	if aEnd == "" {
		aEnd = "9999-12-31"
	}
	if bEnd == "" {
		bEnd = "9999-12-31"
	}
	return a.EffectiveFrom <= bEnd && b.EffectiveFrom <= aEnd
}

func (t *Table) lookupWithKey(model string) (Price, string, bool) {
	if p, ok := t.Models[model]; ok {
		return p, model, true
	}
	best := ""
	for key := range t.Models {
		if strings.HasPrefix(model, key) && versionSuffix(model[len(key):]) && len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return Price{}, "", false
	}
	return t.Models[best], best, true
}
