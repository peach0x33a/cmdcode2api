# Model Exclusion - Decisions

## Matching Semantics
- **Approach**: Prefix matching (no glob/regex)
- **Scope**: Match against raw model name AND suffix after last `/` (for provider-qualified IDs)
- **Empty/nil list**: No exclusions (backward compatible)
- **Whitespace**: Trim entries; skip empty ones

## Filter Strategy
- **Non-destructive**: Keep `modelCatalog` intact; filter at read boundary via `isModelExcluded()`
- **Shared helper**: Same `isModelExcluded()` used by both `/v1/models` filtering and `/v1/chat/completions` blocking

## Config Template
- `yaml.Marshal` can't emit comments → use raw string template for first-run config
- `writeConfigTemplate()` function in `config.go`
- `saveConfig()` unchanged (used by OAuth flow and normal saves)
- Exclusions are **enabled by default** for `gpt-`, `claude-`, and `gemini-` prefixes in `defaultConfig()`.
- First-run template copy must describe the opt-out behavior truthfully: remove entries or set `exclude_models: []` to make all models available.
- Do not include a duplicate commented `# exclude_models:` block when active defaults are serialized by `yaml.Marshal`.

## OpenAI Compatibility
- Excluded chat-completion models return HTTP 404 with the existing `invalid_request_error` JSON shape, matching the OpenAI-style “model unavailable/not found” convention.

## Startup Model Count
- Startup logging reports both loaded catalog size and post-exclusion available count: `models: N loaded, M available`.

## Task Order
- Wave 1: Task 1 + Task 2 (parallel, no deps)
- Wave 2: Task 3 + Task 4 (parallel, depends on Task 1 + 2)
- Wave 3: Task 5 → Task 6 (sequential, depends on Wave 2)
