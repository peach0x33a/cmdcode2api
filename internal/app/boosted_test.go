package app

import (
	"math"
	"testing"
)

func approx(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// A GOAT account on DeepSeek V4.1 Flash: 70.014-credit pool, $100 allowance,
// 35 credits weekly. The weekly window is spent, so its USD remainder is zero
// while the monthly remainder converts to roughly $50.
func TestBoostedForGoat(t *testing.T) {
	snap := &QuotaSnapshot{
		MonthlyCredits: floatPtr(34.9968542434),
		Usage:          &QuotaUsage{TotalCredits: 35.01748727819996},
		FiveHour:       &QuotaWindow{Used: 0.4058471802, Cap: 14},
		Weekly:         &QuotaWindow{Used: 35.0031457566, Cap: 35},
		Plan: &QuotaPlan{
			PlanID:         "individual-goat",
			Name:           "GOAT",
			MonthlyCredits: floatPtr(70.01434152159996),
		},
	}
	view := BoostedFor(snap)
	if view == nil {
		t.Fatal("BoostedFor returned nil for a GOAT snapshot")
	}
	if view.Plan != "goat" || view.StandardUSD != 20 {
		t.Fatalf("plan/standard = %q/%v, want goat/20", view.Plan, view.StandardUSD)
	}
	if view.Standard <= 0 {
		t.Fatalf("standard model count = %d, want > 0", view.Standard)
	}
	if len(view.Models) == 0 {
		t.Fatal("no boosted models")
	}

	var deepseek *BoostedModelView
	for i := range view.Models {
		if view.Models[i].ID == "deepseek-v4.1-flash" {
			deepseek = &view.Models[i]
		}
	}
	if deepseek == nil {
		t.Fatal("deepseek-v4.1-flash missing from the boosted list")
	}
	approx(t, deepseek.AllowanceUSD, 100, "allowance")
	approx(t, deepseek.Factor, 100.0/70.01434152159996, "factor")
	approx(t, deepseek.RemainingUSD, 49.985, "remaining USD")
	if deepseek.FiveHourUSD == nil {
		t.Fatal("five-hour USD missing")
	}
	approx(t, *deepseek.FiveHourUSD, 19.417, "five-hour USD")
	if deepseek.WeeklyUSD == nil || *deepseek.WeeklyUSD != 0 {
		t.Fatalf("weekly USD = %v, want 0 for a spent window", deepseek.WeeklyUSD)
	}

	// Highest allowance first.
	for i := 1; i < len(view.Models); i++ {
		if view.Models[i-1].AllowanceUSD < view.Models[i].AllowanceUSD {
			t.Fatalf("models not sorted by allowance at %d", i)
		}
	}
	// Standard and reduced models stay out of the boosted list.
	for _, m := range view.Models {
		if m.AllowanceUSD <= view.StandardUSD {
			t.Fatalf("%s has allowance %v, not above standard %v", m.ID, m.AllowanceUSD, view.StandardUSD)
		}
	}
}

// A Go account: 10-credit pool, standard allowance $6, boosted models up to $10.
func TestBoostedForGoPlan(t *testing.T) {
	snap := &QuotaSnapshot{
		MonthlyCredits: floatPtr(3.815053954),
		Usage:          &QuotaUsage{TotalCredits: 6.189325638},
		Plan: &QuotaPlan{
			PlanID:         "individual-go-v1",
			Name:           "Go",
			MonthlyCredits: floatPtr(10.004379592),
		},
	}
	view := BoostedFor(snap)
	if view == nil {
		t.Fatal("BoostedFor returned nil for a Go snapshot")
	}
	if view.StandardUSD != 6 {
		t.Fatalf("standard = %v, want 6", view.StandardUSD)
	}
	for _, m := range view.Models {
		if m.ID == "deepseek-v4.1-flash" {
			approx(t, m.AllowanceUSD, 10, "go allowance")
			approx(t, m.RemainingUSD, 3.813, "go remaining USD")
			return
		}
	}
	t.Fatal("deepseek-v4.1-flash missing from the Go boosted list")
}

// A remaining balance above the observed pool (carried or bonus credits) must
// not push a model's estimate past its own allowance.
func TestBoostedForClampsRemaining(t *testing.T) {
	snap := &QuotaSnapshot{
		MonthlyCredits: floatPtr(90),
		Usage:          &QuotaUsage{TotalCredits: 10},
		Plan:           &QuotaPlan{PlanID: "individual-goat", Name: "GOAT", MonthlyCredits: floatPtr(70)},
	}
	view := BoostedFor(snap)
	if view == nil {
		t.Fatal("BoostedFor returned nil")
	}
	top := view.Models[0]
	approx(t, top.RemainingUSD, top.AllowanceUSD, "clamped remaining")
}

// Plans without a per-model allowance table, or snapshots without the numbers
// the conversion needs, produce no tracker at all.
func TestBoostedForSkipsUnknownPlans(t *testing.T) {
	cases := map[string]*QuotaSnapshot{
		"nil snapshot": nil,
		"no plan": {
			MonthlyCredits: floatPtr(10),
		},
		"provider plan": {
			MonthlyCredits: floatPtr(10),
			Plan:           &QuotaPlan{PlanID: "individual-provider", MonthlyCredits: floatPtr(15)},
		},
		"max plan": {
			MonthlyCredits: floatPtr(100),
			Plan:           &QuotaPlan{PlanID: "individual-max", MonthlyCredits: floatPtr(150)},
		},
		"unknown plan": {
			MonthlyCredits: floatPtr(10),
			Plan:           &QuotaPlan{PlanID: "individual-mystery", MonthlyCredits: floatPtr(10)},
		},
		"no pool": {
			MonthlyCredits: floatPtr(10),
			Plan:           &QuotaPlan{PlanID: "individual-goat"},
		},
		"no remaining": {
			Plan: &QuotaPlan{PlanID: "individual-goat", MonthlyCredits: floatPtr(70)},
		},
		// Without the usage aggregate the pool is only the static plan estimate
		// (100 for GOAT), so the conversion must be skipped, not guessed.
		"no usage aggregate": {
			MonthlyCredits: floatPtr(34),
			Plan:           &QuotaPlan{PlanID: "individual-goat", Name: "GOAT", MonthlyCredits: floatPtr(100)},
		},
		"zero pool": {
			MonthlyCredits: floatPtr(0),
			Plan:           &QuotaPlan{PlanID: "individual-goat", MonthlyCredits: floatPtr(0)},
		},
	}
	for name, snap := range cases {
		if view := BoostedFor(snap); view != nil {
			t.Fatalf("%s: expected nil, got %+v", name, view)
		}
	}
}

// Gateway ids carry provider namespaces and date stamps, the catalog does not.
func TestAllowanceLookup(t *testing.T) {
	cases := []struct {
		modelID string
		wantID  string
	}{
		{"deepseek/deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"z-ai/glm-5.3-flash", "glm-5.3-flash"},
		{"inclusionai/ling-3.1-flash", "ling-3.1-flash"},
		{"Qwen/Qwen3.8-Max-0902", "qwen-3.8-max-0902"},
		{"MiniMaxAI/MiniMax-M2.5", "minimax-m2.5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
	}
	for _, tc := range cases {
		entry, ok := allowanceFor(tc.modelID)
		if !ok {
			t.Fatalf("%s: not found", tc.modelID)
		}
		if entry.ID != tc.wantID {
			t.Fatalf("%s: matched %s, want %s", tc.modelID, entry.ID, tc.wantID)
		}
	}
	if _, ok := allowanceFor("not/a-real-model"); ok {
		t.Fatal("unknown model matched an allowance entry")
	}
}
