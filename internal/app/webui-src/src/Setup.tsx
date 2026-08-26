import { useCallback, useEffect, useState } from "react";
import { fetchConnection } from "./api";
import CopyField from "./CopyField";
import { buildClientGuides } from "./clients";
import { errorMessage } from "./reauth";
import { useCopyToClipboard } from "./useCopyToClipboard";
import type { ConnectionInfo } from "./types";

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
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const data = await fetchConnection();
        if (!cancelled) {
          setConn(data);
          setError("");
        }
      } catch (err) {
        if (!cancelled) {
          setError("Failed to load connection info: " + errorMessage(err));
        }
      } finally {
        if (!cancelled) {
          setLoading(false);
        }
      }
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
            <CopyField label="API Key" value={conn.api_key} secret />
          </div>

          {buildClientGuides(conn).map((guide) => (
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
