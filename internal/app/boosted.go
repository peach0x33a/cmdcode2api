package app

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// Command Code publishes a per-model monthly allowance per plan (the "boosted
// usage" deals). The pool is charged in plan credits, so one model's dollars
// per credit differ from another's: GOAT grants a 70-credit pool and, on
// DeepSeek V4.1 Flash, a $100 allowance (1 credit buys $1.43 of usage).
// allowances.json is captured from the docs model catalog; it is reference
// data, not configuration, and the upstream credit numbers stay authoritative.
//
//go:embed allowances.json
var allowancesJSON []byte

// allowanceTable is the captured catalog plus the per-plan standard allowance.
type allowanceTable struct {
	Source   string `json:"source"`
	Captured string `json:"captured"`
	Models   []struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Category string  `json:"category"`
		Go       float64 `json:"go"`
		Goat     float64 `json:"goat"`
		Pro      float64 `json:"pro"`
	} `json:"models"`
}

// allowanceEntry is one model's monthly allowance for the plan in use.
type allowanceEntry struct {
	ID       string
	Name     string
	Category string
	Go       float64
	Goat     float64
	Pro      float64
}

// standardAllowance is the allowance shared by the plan's non-boosted models.
// Anything above it is a boosted allowance worth showing to the operator.
var standardAllowance = map[string]float64{"go": 6, "goat": 20, "pro": 30}

var (
	allowanceOnce  sync.Once
	allowanceIndex map[string]allowanceEntry
)

// BoostedModelView is one model's remaining allowance, expressed both in plan
// credits (authoritative) and in the model's own USD allowance (derived).
type BoostedModelView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	AllowanceUSD float64  `json:"allowance_usd"`
	Factor       float64  `json:"factor"`
	RemainingUSD float64  `json:"remaining_usd"`
	FiveHourUSD  *float64 `json:"five_hour_usd,omitempty"`
	WeeklyUSD    *float64 `json:"weekly_usd,omitempty"`
}

// BoostedView is the per-account boosted-allowance tracker. Models lists the
// boosted models of the account's plan, highest allowance first. StandardUSD is
// the plan's baseline per-model allowance and StandardModels how many catalog
// models sit at that baseline, so the UI can summarise without dumping them.
type BoostedView struct {
	Plan        string             `json:"plan"`
	PoolCredits float64            `json:"pool_credits"`
	StandardUSD float64            `json:"standard_usd"`
	Standard    int                `json:"standard_models"`
	Models      []BoostedModelView `json:"models"`
	SourceURL   string             `json:"source_url"`
	SourceDate  string             `json:"source_captured"`
}

func initAllowances() {
	allowanceOnce.Do(func() {
		allowanceIndex = map[string]allowanceEntry{}
		var table allowanceTable
		if err := json.Unmarshal(allowancesJSON, &table); err != nil {
			return
		}
		for _, m := range table.Models {
			entry := allowanceEntry{ID: m.ID, Name: m.Name, Category: m.Category, Go: m.Go, Goat: m.Goat, Pro: m.Pro}
			for _, key := range []string{normalizeModelID(m.ID), normalizeModelID(m.Name)} {
				if key == "" {
					continue
				}
				if _, taken := allowanceIndex[key]; !taken {
					allowanceIndex[key] = entry
				}
			}
			allowanceIndex["#"+m.ID] = entry
			allowanceSourceURL = table.Source
			allowanceSourceDate = table.Captured
		}
	})
}

var (
	allowanceSourceURL  string
	allowanceSourceDate string
)

// normalizeModelID makes provider-prefixed gateway ids ("deepseek/deepseek-v4.1-
// flash", "Qwen/Qwen3.8-Max-0902") and catalog ids ("deepseek-v4.1-flash")
// comparable: lowercase, namespace removed, punctuation dropped.
func normalizeModelID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// allowanceFor resolves a gateway model id to its catalog entry. Exact id and
// display-name matches win; a catalog id that is a prefix of the gateway id
// covers date-stamped variants such as claude-haiku-4-5-20251001.
func allowanceFor(modelID string) (allowanceEntry, bool) {
	initAllowances()
	if len(allowanceIndex) == 0 {
		return allowanceEntry{}, false
	}
	key := normalizeModelID(modelID)
	if entry, ok := allowanceIndex[key]; ok {
		return entry, true
	}
	raw := strings.ToLower(strings.TrimSpace(modelID))
	if entry, ok := allowanceIndex["#"+raw]; ok {
		return entry, true
	}
	best := ""
	var bestEntry allowanceEntry
	for candidate, entry := range allowanceIndex {
		if strings.HasPrefix(candidate, "#") || candidate == "" || !strings.HasPrefix(key, candidate) {
			continue
		}
		if len(candidate) > len(best) {
			best, bestEntry = candidate, entry
		}
	}
	if best == "" {
		return allowanceEntry{}, false
	}
	return bestEntry, true
}

// planTier maps a planId to the catalog's plan column. Provider/Ultra/Max plans
// have no per-model table here, so they fall through to no tracker.
func planTier(planID string) string {
	id := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(planID), "_", "-"))
	switch {
	case strings.HasPrefix(id, "individual-provider"):
		return ""
	case strings.HasPrefix(id, "individual-goat"):
		return "goat"
	case strings.HasPrefix(id, "individual-go"):
		return "go"
	case strings.HasPrefix(id, "individual-pro"):
		return "pro"
	}
	return ""
}

func allowanceForTier(entry allowanceEntry, tier string) float64 {
	switch tier {
	case "go":
		return entry.Go
	case "goat":
		return entry.Goat
	case "pro":
		return entry.Pro
	}
	return 0
}

// BoostedFor derives the boosted-model tracker from a cached quota snapshot.
// The credit numbers come from upstream; only the USD conversion uses the
// captured catalog, and it is labelled as an estimate in the UI. A nil result
// means the plan has no per-model allowance table (Provider, Max, unknown) or
// the snapshot lacks the pool and remaining balance.
func BoostedFor(snap *QuotaSnapshot) *BoostedView {
	if snap == nil || snap.Plan == nil || snap.Plan.MonthlyCredits == nil || snap.MonthlyCredits == nil {
		return nil
	}
	// Without the usage aggregate the pool is only the community CLI's static
	// estimate (100 credits for GOAT) rather than the observed allowance
	// (70), which would skew every factor by ~30%. Show no tracker instead of
	// a wrong one.
	if snap.Usage == nil {
		return nil
	}
	tier := planTier(snap.Plan.PlanID)
	if tier == "" {
		return nil
	}
	pool := *snap.Plan.MonthlyCredits
	remaining := *snap.MonthlyCredits
	if pool <= 0 || remaining < 0 {
		return nil
	}
	if remaining > pool {
		remaining = pool
	}
	standard, ok := standardAllowance[tier]
	if !ok {
		return nil
	}

	initAllowances()
	view := &BoostedView{
		Plan:        tier,
		PoolCredits: pool,
		StandardUSD: standard,
		SourceURL:   allowanceSourceURL,
		SourceDate:  allowanceSourceDate,
	}
	seen := map[string]bool{}
	for _, entry := range allowanceIndex {
		if strings.HasPrefix(entry.ID, "#") || seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		allowance := allowanceForTier(entry, tier)
		if allowance <= 0 {
			continue
		}
		if allowance <= standard {
			if allowance == standard {
				view.Standard++
			}
			continue
		}
		factor := allowance / pool
		model := BoostedModelView{
			ID:           entry.ID,
			Name:         cleanModelName(entry.Name),
			AllowanceUSD: allowance,
			Factor:       factor,
			RemainingUSD: remaining * factor,
		}
		model.FiveHourUSD = windowUSD(snap.FiveHour, factor)
		model.WeeklyUSD = windowUSD(snap.Weekly, factor)
		view.Models = append(view.Models, model)
	}
	sort.Slice(view.Models, func(i, j int) bool {
		if view.Models[i].AllowanceUSD != view.Models[j].AllowanceUSD {
			return view.Models[i].AllowanceUSD > view.Models[j].AllowanceUSD
		}
		return view.Models[i].Name < view.Models[j].Name
	})
	if len(view.Models) == 0 {
		return nil
	}
	return view
}

// windowUSD converts a window's remaining credits to the model's USD allowance.
func windowUSD(w *QuotaWindow, factor float64) *float64 {
	if w == nil || w.Cap <= 0 {
		return nil
	}
	left := w.Cap - w.Used
	if left < 0 {
		left = 0
	}
	v := left * factor
	return &v
}

// cleanModelName drops the "(latest)" suffix the catalog adds to current ids.
func cleanModelName(name string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), "(latest)"))
}
