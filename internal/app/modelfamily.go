package app

import "strings"

// ModelGrouping is the display/grouping-only decomposition of a catalog
// model ID into its family and variant.
type ModelGrouping struct {
	Family      string
	FamilyLabel string
	Variant     string
}

// groupModel derives id's family/label/variant by splitting at the first "/"
// or "-", whichever occurs first. If neither delimiter is present, the
// complete ID is the family and the variant is empty.
func groupModel(id string) ModelGrouping {
	if id == "" {
		return ModelGrouping{}
	}

	family := id
	variant := ""
	if idx := strings.IndexAny(id, "/-"); idx >= 0 {
		family = id[:idx]
		variant = id[idx+1:]
	}

	return ModelGrouping{
		Family:      family,
		FamilyLabel: prettifySegment(family),
		Variant:     variant,
	}
}

// prettifySegment turns a hyphen/underscore-separated identifier segment
// into a human-readable label. It is display-only, never a key comparison.
func prettifySegment(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		parts[i] = string(r)
	}
	return strings.Join(parts, " ")
}
