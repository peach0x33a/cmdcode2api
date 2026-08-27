import type { AdminModel, ConnectionInfo } from "./types";

export interface ClientGuide {
  id: string;
  title: string;
  steps: string[];
  snippets: { label: string; language: string; code: string }[];
  note?: string; // for caveats/warnings
}

// OS is the caller's operating system. It selects the config-file locations
// and the shell syntax each guide uses for setting environment variables.
export type OS = "windows" | "mac" | "linux";

export const OS_ORDER: OS[] = ["mac", "linux", "windows"];

export const OS_LABELS: Record<OS, string> = {
  mac: "macOS",
  linux: "Linux",
  windows: "Windows",
};

// detectOS guesses the current OS from the browser's navigator, falling back
// to Linux when nothing matches (the common self-hosted/headless case). The
// Setup UI lets the user override the guess.
export function detectOS(): OS {
  const nav = typeof navigator !== "undefined" ? navigator : undefined;
  const hint = (
    (nav as { userAgentData?: { platform?: string } } | undefined)?.userAgentData?.platform ||
    nav?.platform ||
    nav?.userAgent ||
    ""
  ).toLowerCase();
  if (hint.includes("win")) return "windows";
  if (
    hint.includes("mac") ||
    hint.includes("darwin") ||
    hint.includes("iphone") ||
    hint.includes("ipad")
  ) {
    return "mac";
  }
  return "linux";
}

const PLACEHOLDER_MODEL_ID = "<model-id-from-models-tab>";
const CODEX_PLACEHOLDER_MODEL_ID = "<a model from the Models tab>";

// Shown wherever a guide exposes a reasoning-effort knob, so the accepted
// values travel with the snippet that uses them.
const REASONING_EFFORT_NOTE =
  'Claude and GPT models take a reasoning effort — "low", "medium", or "high" ("minimal" is GPT-only). Drop the setting to use the model default.';

// codexConfigPath / opencodeConfigPath return the platform-correct location
// of each client's config file: "~" and forward slashes on Unix,
// %USERPROFILE% and backslashes on Windows.
function codexConfigPath(os: OS): string {
  return os === "windows" ? "%USERPROFILE%\\.codex\\config.toml" : "~/.codex/config.toml";
}

function opencodeConfigPath(os: OS): string {
  return os === "windows"
    ? "%USERPROFILE%\\.config\\opencode\\opencode.json"
    : "~/.config/opencode/opencode.json";
}

// envVarSnippet renders the shell commands that set each name=value pair for
// the caller's OS: `export` for macOS/Linux, `$env:` (current session) plus
// `setx` (persisted) for Windows PowerShell.
function envVarSnippet(
  os: OS,
  vars: { name: string; value: string }[]
): { label: string; language: string; code: string } {
  if (os === "windows") {
    return {
      label: "PowerShell",
      language: "powershell",
      code: vars
        .map(
          (v) =>
            `$env:${v.name} = "${v.value}"      # current session\nsetx ${v.name} "${v.value}"        # persist for new sessions`
        )
        .join("\n"),
    };
  }
  return {
    label: os === "mac" ? "shell (zsh)" : "shell (bash)",
    language: "bash",
    code: vars.map((v) => `export ${v.name}="${v.value}"`).join("\n"),
  };
}

// isReasoningModel reports whether m is a Claude or GPT model — the families
// that honour a reasoning-effort setting. It checks the derived family and
// the raw ID, since catalog IDs may or may not carry a provider prefix.
function isReasoningModel(m: AdminModel): boolean {
  const id = m.id.toLowerCase();
  const fam = m.family.toLowerCase();
  return (
    fam === "anthropic" ||
    fam === "openai" ||
    fam === "claude" ||
    fam === "gpt" ||
    id.includes("claude") ||
    id.includes("gpt-") ||
    id.includes("gpt5")
  );
}

// REASONING_EFFORT_VARIANTS are the OpenCode model "variants" attached to
// every enabled Claude/GPT model, so the reader can pick an effort with
// `opencode --model cmdcdswitcher/<id>:high` instead of editing config.
const REASONING_EFFORT_VARIANTS = ["medium", "high", "max"] as const;

// buildOpencodeModelsBlock renders one entry per model for the opencode.json
// "models" object — every variant of every enabled family appears
// individually, since each variant is its own catalog model ID. Claude and
// GPT models also carry a "variants" object exposing medium/high/max
// reasoning-effort presets. Falls back to the placeholder entry when models
// is empty (e.g. a fresh install with no accounts yet).
function buildOpencodeModelsBlock(models: AdminModel[]): string {
  if (models.length === 0) {
    return `        ${JSON.stringify(PLACEHOLDER_MODEL_ID)}: { "name": ${JSON.stringify(PLACEHOLDER_MODEL_ID)} }`;
  }
  return models
    .map((m) => {
      const name = JSON.stringify(m.name || m.id);
      const key = JSON.stringify(m.id);
      if (!isReasoningModel(m)) {
        return `        ${key}: { "name": ${name} }`;
      }
      const variants = REASONING_EFFORT_VARIANTS.map(
        (v) => `            ${JSON.stringify(v)}: { "reasoningEffort": ${JSON.stringify(v)} }`
      ).join(",\n");
      return `        ${key}: {
          "name": ${name},
          "variants": {
${variants}
          }
        }`;
    })
    .join(",\n");
}

// buildClientGuides returns the setup instructions for every client this
// gateway is documented to work with, with conn's real base URL and API key
// substituted directly into each snippet. models is the caller's
// already-filtered list of currently *enabled* models, sorted by family then
// id — used to populate the OpenCode provider's model list and the
// Codex/OpenCode default model, falling back to a placeholder when it's empty
// (e.g. before the first account/model is available). os tailors config-file
// paths and env-var syntax to the reader's platform, and any enabled Claude
// or GPT model adds the reasoning-effort knobs each client exposes.
export function buildClientGuides(
  conn: ConnectionInfo,
  models: AdminModel[],
  os: OS
): ClientGuide[] {
  const firstModelID = models.length > 0 ? models[0].id : CODEX_PLACEHOLDER_MODEL_ID;
  // Per OpenCode's own docs (https://opencode.ai/v2/docs/models), OpenCode
  // splits a "provider/modelID" reference at the first "/" only, so a
  // modelID that itself contains further slashes (e.g.
  // "deepseek/deepseek-v4-pro") resolves correctly as provider
  // "cmdcdswitcher", modelID "deepseek/deepseek-v4-pro".
  const opencodeTopLevelModel =
    models.length > 0 ? `cmdcdswitcher/${models[0].id}` : `cmdcdswitcher/${PLACEHOLDER_MODEL_ID}`;

  const opencodeLastStep =
    models.length > 0
      ? `All ${models.length} enabled model${models.length === 1 ? "" : "s"} are listed below — the top-level "model" defaults to the first one.`
      : "Select the model.";

  const reasoning = models.some(isReasoningModel);

  // Inserted right after `model = "..."` in config.toml when a Claude/GPT
  // model is enabled; empty otherwise so the blank line before the provider
  // table is preserved.
  const codexReasoningLines = reasoning
    ? `\n# ${REASONING_EFFORT_NOTE}\nmodel_reasoning_effort = "high"\nmodel_reasoning_summary = "auto"\n`
    : "";

  return [
    {
      id: "codex",
      title: "Codex CLI",
      note: 'Codex CLI requires the OpenAI Responses API (`wire_api = "responses"`) for custom providers, which this proxy implements at /v1/responses.',
      steps: [
        `Edit ${codexConfigPath(os)}.`,
        "Add the complete configuration below and save the file.",
        "Set the CMDCDSWITCHER_API_KEY environment variable.",
        "Run: codex",
      ],
      snippets: [
        {
          label: codexConfigPath(os),
          language: "toml",
          code: `model_provider = "cmdcdswitcher"
model = "${firstModelID}"
${codexReasoningLines}
[model_providers.cmdcdswitcher]
name = "CmdCdSwitcher"
base_url = "${conn.base_url}"
wire_api = "responses"
env_key = "CMDCDSWITCHER_API_KEY"
`,
        },
        envVarSnippet(os, [{ name: "CMDCDSWITCHER_API_KEY", value: conn.api_key }]),
      ],
    },
    {
      id: "openai-generic",
      title: "Generic OpenAI-compatible client",
      steps: [
        "Many tools/SDKs built on the OpenAI SDK accept these two environment variables directly.",
        "Check your tool's docs for the exact variable names it reads.",
        ...(reasoning
          ? [`${REASONING_EFFORT_NOTE} Send it as the "reasoning_effort" request field.`]
          : []),
      ],
      snippets: [
        envVarSnippet(os, [
          { name: "OPENAI_BASE_URL", value: conn.base_url },
          { name: "OPENAI_API_KEY", value: conn.api_key },
        ]),
      ],
    },
    {
      id: "opencode",
      title: "OpenCode",
      note: "One open OpenCode issue (anomalyco/opencode#5674) reports custom-provider options sometimes not being passed through in some versions — if the connection fails, confirm you're on a current OpenCode release.",
      steps: [
        `Create/edit opencode.json (project root or ${opencodeConfigPath(os)}).`,
        "Add the provider block below.",
        "Set the CMDCDSWITCHER_API_KEY environment variable.",
        opencodeLastStep,
        ...(reasoning
          ? [
              `${REASONING_EFFORT_NOTE} Each Claude/GPT model below defines medium/high/max variants — select one with \`cmdcdswitcher/<id>:high\`.`,
            ]
          : []),
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
${buildOpencodeModelsBlock(models)}
      }
    }
  },
  "model": "${opencodeTopLevelModel}"
}
`,
        },
        envVarSnippet(os, [{ name: "CMDCDSWITCHER_API_KEY", value: conn.api_key }]),
      ],
    },
  ];
}
