import { Copy, X } from "lucide-react";
import { Modal } from "./Modal";

const endpoints = [
  { method: "GET", path: "/models", note: "List the models a key can use" },
  { method: "POST", path: "/responses", note: "OpenAI Responses API" },
  { method: "POST", path: "/messages", note: "Anthropic Messages API" },
];

export function HelpDialog({
  publicUrl,
  copy,
  onClose,
}: {
  publicUrl: string;
  copy: (value: string) => void;
  onClose: () => void;
}) {
  const base = `${publicUrl || location.origin}/v1`;
  const example = `curl ${base}/models \\\n  -H "Authorization: Bearer $VROUTER_API_KEY"`;
  return (
    <Modal className="help-dialog" labelledBy="help-title" onClose={onClose}>
      <div className="dialog-heading">
        <h2 id="help-title">Client API</h2>
        <button
          className="icon-button"
          aria-label="Close help"
          onClick={onClose}
        >
          <X size={18} />
        </button>
      </div>
      <p className="dialog-lead">
        Point a client at the base URL and authenticate with an API key from the
        Keys page.
      </p>
      <div className="copy-field">
        <code>{base}</code>
        <button
          className="icon-button"
          aria-label="Copy base URL"
          onClick={() => copy(base)}
        >
          <Copy size={14} />
        </button>
      </div>
      <ul className="endpoint-list">
        {endpoints.map((e) => (
          <li key={e.path}>
            <span className="endpoint-method">{e.method}</span>
            <code>/v1{e.path}</code>
            <span className="endpoint-note">{e.note}</span>
            <button
              className="icon-button"
              aria-label={`Copy ${e.path.slice(1)} URL`}
              onClick={() => copy(base + e.path)}
            >
              <Copy size={14} />
            </button>
          </li>
        ))}
      </ul>
      <div className="copy-field block">
        <pre>{example}</pre>
        <button
          className="icon-button"
          aria-label="Copy example request"
          onClick={() => copy(example)}
        >
          <Copy size={14} />
        </button>
      </div>
    </Modal>
  );
}
