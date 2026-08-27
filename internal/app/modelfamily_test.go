package app

import "testing"

func TestGroupModelSplitsAtFirstDelimiter(t *testing.T) {
	tests := []struct {
		id, family, variant string
	}{
		{"deepseek/deepseek-v4-pro", "deepseek", "deepseek-v4-pro"},
		{"gpt-4/openai", "gpt", "4/openai"},
		{"mistral/mixtral", "mistral", "mixtral"},
		{"gpt-4", "gpt", "4"},
		{"model/", "model", ""},
		{"-variant", "", "variant"},
		{"model--variant", "model", "-variant"},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			g := groupModel(tt.id)
			if g.Family != tt.family || g.Variant != tt.variant {
				t.Fatalf("groupModel(%q) = Family %q, Variant %q; want %q, %q", tt.id, g.Family, g.Variant, tt.family, tt.variant)
			}
		})
	}
}

func TestGroupModelNoDelimiter(t *testing.T) {
	g := groupModel("mixtral")
	if g.Family != "mixtral" || g.Variant != "" {
		t.Fatalf("groupModel(%q) = %#v, want family mixtral and empty variant", "mixtral", g)
	}
}

func TestGroupModelEmptyString(t *testing.T) {
	g := groupModel("")
	if g.Family != "" || g.FamilyLabel != "" || g.Variant != "" {
		t.Fatalf("groupModel(\"\") = %#v, want zero value", g)
	}
}

func TestGroupModelFamilyLabelIsHumanReadable(t *testing.T) {
	g := groupModel("deepseek/deepseek-v4-pro")
	if g.FamilyLabel != "Deepseek" {
		t.Fatalf("FamilyLabel = %q, want %q", g.FamilyLabel, "Deepseek")
	}
}
