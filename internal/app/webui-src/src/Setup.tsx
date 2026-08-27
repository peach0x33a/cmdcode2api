import { useCallback, useEffect, useState } from "react";
import { fetchConnection, fetchModels } from "./api";
import CopyField, { MASK } from "./CopyField";
import { buildClientGuides, detectOS, OS_LABELS, OS_ORDER, type OS } from "./clients";
import { errorMessage } from "./reauth";
import { useCopyToClipboard } from "./useCopyToClipboard";
import type { AdminModel, ConnectionInfo } from "./types";

// enabledModelsSortedByFamily filters to currently-enabled models and sorts
// them by family then id, so buildClientGuides gets a stable, grouped order
// for the OpenCode provider's model list.
function enabledModelsSortedByFamily(models: AdminModel[]): AdminModel[] {
  return models
    .filter((m) => !m.excluded)
    .slice()
    .sort((a, b) => a.family.localeCompare(b.family) || a.id.localeCompare(b.id));
}

function CodeBlock({ code, language }: { code: string; language: string }) {
  const [copied, copy] = useCopyToClipboard();

  const handleCopy = useCallback(() => copy(code), [copy, code]);

  return (
    <div className="code-block-wrap">
      <pre className="code-block">
        <code className={`language-${language}`}>{code}</code>
      </pre>
      <button type="button" className="code-block-copy" onClick={handleCopy}>
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

export default function Setup() {
  const [conn, setConn] = useState<ConnectionInfo | null>(null);
  const [models, setModels] = useState<AdminModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [keyRevealed, setKeyRevealed] = useState(false);
  const [os, setOs] = useState<OS>(detectOS);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      // The models fetch is best-effort: a failure here degrades the
      // OpenCode guide to its placeholder fallback rather than breaking the
      // whole Setup tab — mirroring how Accounts.tsx treats its secondary
      // billing fetch.
      const [connResult, modelsResult] = await Promise.allSettled([fetchConnection(), fetchModels()]);
      if (cancelled) return;

      if (connResult.status === "fulfilled") {
        setConn(connResult.value);
        setError("");
      } else {
        setError("Failed to load connection info: " + errorMessage(connResult.reason));
      }
      setModels(modelsResult.status === "fulfilled" ? modelsResult.value : []);
      setLoading(false);
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div>
      <p className="subtitle">Connect an external client to this gateway.</p>

      {error ? <div className="banner error">{error}</div> : null}

      {loading ? (
        <p style={{ padding: "1rem" }}>Loading…</p>
      ) : conn ? (
        <>
          <div className="card guide-card">
            <h2>Connection</h2>
            <CopyField label="Base URL" value={conn.base_url} />
            {conn.lan_base_url ? <CopyField label="LAN Base URL" value={conn.lan_base_url} /> : null}
            {conn.tailscale_base_url ? (
              <CopyField label="Tailscale Base URL" value={conn.tailscale_base_url} />
            ) : null}
            <CopyField
              label="API Key"
              value={conn.api_key}
              secret
              revealed={keyRevealed}
              onToggleRevealed={() => setKeyRevealed((r) => !r)}
            />
          </div>

          <div className="card guide-card">
            <h2>Operating system</h2>
            <div className="snippet-label" style={{ marginBottom: "0.5rem" }}>
              Config paths and shell commands below are tailored to this choice.
            </div>
            <div className="tabs" role="tablist">
              {OS_ORDER.map((id) => (
                <button
                  key={id}
                  type="button"
                  role="tab"
                  aria-selected={os === id}
                  className={`tab${os === id ? " active" : ""}`}
                  onClick={() => setOs(id)}
                >
                  {OS_LABELS[id]}
                </button>
              ))}
            </div>
          </div>

          {buildClientGuides(
            keyRevealed ? conn : { ...conn, api_key: MASK },
            enabledModelsSortedByFamily(models),
            os
          ).map((guide) => (
            <div className="card guide-card" key={guide.id}>
              <h2>{guide.title}</h2>
              {guide.note ? <div className="callout warning">{guide.note}</div> : null}
              <ol>
                {guide.steps.map((step, i) => (
                  <li key={i}>{step}</li>
                ))}
              </ol>
              {guide.snippets.map((snippet, i) => (
                <div key={i} className="snippet">
                  <div className="snippet-label">{snippet.label}</div>
                  <CodeBlock code={snippet.code} language={snippet.language} />
                </div>
              ))}
            </div>
          ))}
        </>
      ) : null}
    </div>
  );
}
