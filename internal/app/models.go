package app

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

var modelCatalog []ModelInfo

// modelCatalogDetail carries the fields CCProviderModel has that ModelInfo
// discards (Name, ContextLength), for the web UI's Models tab. modelCatalog
// has no lock or atomic swap of its own — FetchProviderModels only ever runs
// once, at startup, before any request-serving goroutine reads it — so
// modelCatalogDetail follows that same discipline. What matters is that both
// vars are built in the same loop and assigned right next to each other (see
// FetchProviderModels), so a reader never sees one updated without the other.
var modelCatalogDetail []CCProviderModel

// FetchProviderModels fetches the model list from the CC API and populates modelCatalog.
func FetchProviderModels(baseURL, apiKey string) {
	url := baseURL + "/provider/v1/models"

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("[WARN] fetch models: create request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[WARN] fetch models: %v (using empty catalog)", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[WARN] fetch models: http %d (using empty catalog)", resp.StatusCode)
		return
	}

	var list CCProviderModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		log.Printf("[WARN] fetch models: decode: %v (using empty catalog)", err)
		return
	}

	catalog := make([]ModelInfo, 0, len(list.Data))
	detail := make([]CCProviderModel, 0, len(list.Data))
	for _, m := range list.Data {
		catalog = append(catalog, ModelInfo{
			ID:            m.ID,
			Object:        "model",
			Created:       1700000000,
			OwnedBy:       "commandcode",
			ContextWindow: m.ContextLength,
		})
		detail = append(detail, m)
	}
	modelCatalog = catalog
	modelCatalogDetail = detail
	log.Printf("models: %d loaded from %s", len(modelCatalog), url)
}

// adminModels builds the web UI's Models tab view from modelCatalogDetail,
// flagging each model's current enabled/excluded status per policy and
// filling in its family/variant grouping (see groupModel) and whether an
// explicit per-model override determined that status.
func adminModels(policy *ModelPolicy) []AdminModel {
	out := make([]AdminModel, 0, len(modelCatalogDetail))
	for _, m := range modelCatalogDetail {
		grouping := groupModel(m.ID)
		enabled, overridden := policy.Describe(m.ID, grouping.Family)
		out = append(out, AdminModel{
			ID:            m.ID,
			Name:          m.Name,
			ContextLength: m.ContextLength,
			OwnedBy:       "commandcode",
			Excluded:      !enabled,
			Family:        grouping.Family,
			FamilyLabel:   grouping.FamilyLabel,
			Variant:       grouping.Variant,
			Overridden:    overridden,
		})
	}
	return out
}

func availableModels() []string {
	out := make([]string, 0, len(modelCatalog))
	for _, model := range modelCatalog {
		out = append(out, model.ID)
	}
	return out
}

// knownModelIDs returns the set of model IDs currently in the catalog, for
// validating a /admin/models/toggle request body against real models. An
// empty catalog (e.g. before the first account is added) returns an empty
// set — callers should skip validation entirely in that case rather than
// rejecting every model ID as unknown.
func knownModelIDs() map[string]bool {
	out := make(map[string]bool, len(modelCatalogDetail))
	for _, m := range modelCatalogDetail {
		out[m.ID] = true
	}
	return out
}

func isModelExcluded(model string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	candidates := []string{model}
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		candidates = append(candidates, model[idx+1:])
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		for _, e := range excludes {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if strings.HasPrefix(c, e) {
				return true
			}
		}
	}
	return false
}
