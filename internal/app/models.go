package app

var modelCatalog = []ModelInfo{
	// Premium
	{ID: "claude-opus-4-7", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "claude-opus-4-6", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "claude-sonnet-4-6", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "claude-haiku-4-5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "gpt-5.5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "gpt-5.4", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "gpt-5.3-codex", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "gpt-5.4-mini", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "gemini-3.5-flash", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "gemini-3.1-flash-lite", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	// Open-source
	{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "moonshotai/Kimi-K2.6", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "moonshotai/Kimi-K2.5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "zai-org/GLM-5.1", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "zai-org/GLM-5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "Qwen/Qwen3.6-Plus", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "Qwen/Qwen3.7-Max", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
	{ID: "step-3.5-flash", Object: "model", Created: 1700000000, OwnedBy: "commandcode"},
}

func availableModels() []string {
	out := make([]string, 0, len(modelCatalog))
	for _, model := range modelCatalog {
		out = append(out, model.ID)
	}
	return out
}
