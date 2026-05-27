package app

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

var modelCatalog []ModelInfo

// FetchProviderModels 从 CC API 拉取模型列表，填充 modelCatalog。
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
	for _, m := range list.Data {
		catalog = append(catalog, ModelInfo{
			ID:      m.ID,
			Object:  "model",
			Created: 1700000000,
			OwnedBy: "commandcode",
		})
	}
	modelCatalog = catalog
	log.Printf("models: %d loaded from %s", len(modelCatalog), url)
}

func availableModels() []string {
	out := make([]string, 0, len(modelCatalog))
	for _, model := range modelCatalog {
		out = append(out, model.ID)
	}
	return out
}
