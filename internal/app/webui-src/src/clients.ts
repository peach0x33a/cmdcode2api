import type { ConnectionInfo } from "./types";

export interface ClientGuide {
  id: string;
  title: string;
  steps: string[];
  snippets: { label: string; language: string; code: string }[];
  note?: string; // for caveats/warnings
}

// buildClientGuides returns the setup instructions for every client this
// gateway is documented to work with, with conn's real base URL and API key
// substituted directly into each snippet.
export function buildClientGuides(conn: ConnectionInfo): ClientGuide[] {
  return [
    {
      id: "codex",
      title: "Codex CLI",
      note: 'Codex CLI (as of early 2026) requires the OpenAI Responses API (`wire_api = "responses"`) for custom providers. This proxy currently only implements the Chat Completions API, so Codex CLI cannot connect to it yet.',
      steps: [
        "Edit ~/.codex/config.toml.",
        "Add the provider block below.",
        "Set the CMDCDSWITCHER_API_KEY environment variable.",
        "Run: codex -c model_provider=cmdcdswitcher",
      ],
      snippets: [
        {
          label: "~/.codex/config.toml",
          language: "toml",
          code: `model_provider = "cmdcdswitcher"
model = "<a model from the Models tab>"

[model_providers.cmdcdswitcher]
name = "CmdCdSwitcher"
base_url = "${conn.base_url}"
env_key = "CMDCDSWITCHER_API_KEY"
wire_api = "responses"
`,
        },
        {
          label: "shell",
          language: "bash",
          code: `export CMDCDSWITCHER_API_KEY=${conn.api_key}`,
        },
      ],
    },
    {
      id: "openai-generic",
      title: "Generic OpenAI-compatible client",
      steps: [
        "Many tools/SDKs built on the OpenAI SDK accept these two environment variables directly.",
        "Check your tool's docs for the exact variable names it reads.",
      ],
      snippets: [
        {
          label: "shell",
          language: "bash",
          code: `export OPENAI_BASE_URL="${conn.base_url}"
export OPENAI_API_KEY="${conn.api_key}"`,
        },
      ],
    },
    {
      id: "opencode",
      title: "OpenCode",
      note: "One open OpenCode issue (anomalyco/opencode#5674) reports custom-provider options sometimes not being passed through in some versions — if the connection fails, confirm you're on a current OpenCode release.",
      steps: [
        "Create/edit opencode.json (project root or ~/.config/opencode/opencode.json).",
        "Add the provider block below.",
        "Set the CMDCDSWITCHER_API_KEY environment variable.",
        "Select the model.",
      ],
      snippets: [
        {
          label: "opencode.json",
          language: "json",
          code: `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "cmdcdswitcher": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "CmdCdSwitcher",
      "options": {
        "baseURL": "${conn.base_url}",
        "apiKey": "{env:CMDCDSWITCHER_API_KEY}"
      },
      "models": {
        "<model-id-from-models-tab>": { "name": "<model-id-from-models-tab>" }
      }
    }
  },
  "model": "cmdcdswitcher/<model-id-from-models-tab>"
}
`,
        },
        {
          label: "shell",
          language: "bash",
          code: `export CMDCDSWITCHER_API_KEY=${conn.api_key}`,
        },
      ],
    },
  ];
}
