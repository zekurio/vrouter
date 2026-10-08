import { useState } from "react";
import { Copy, X } from "lucide-react";
import { CodeBlock } from "./CodeBlock";
import { Modal } from "./Modal";
import {
  curlExample,
  protocolPath,
  protocols,
  type Protocol,
} from "./protocols";
import { Select } from "./Select";

const endpoints = [
  { method: "GET", path: "/models", note: "List the models a key can use" },
  { method: "POST", path: "/responses", note: "OpenAI Responses API" },
  {
    method: "POST",
    path: "/chat/completions",
    note: "OpenAI Chat Completions API",
  },
  { method: "POST", path: "/messages", note: "Anthropic Messages API" },
];

export function HelpDialog({
  publicUrl,
  copy,
  onClose,
}: {
  publicUrl: string;
  copy: (value: string) => Promise<boolean>;
  onClose: () => void;
}) {
  const [protocol, setProtocol] = useState<Protocol>("responses");
  const [provider, setProvider] = useState("claude");
  const origin = publicUrl || location.origin;
  const base = `${origin}/v1`;
  const addresses = [
    { label: "OpenAI-compatible clients", value: base },
    { label: "Anthropic clients", value: origin },
  ];
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
        Give your client a base URL and an API key from the Keys page. Choose
        any of these protocols for Claude or Codex. Provider limits still apply.
      </p>
      {addresses.map((a) => (
        <div className="address-field" key={a.label}>
          <span>{a.label}</span>
          <div className="copy-field">
            <code>{a.value}</code>
            <button
              className="icon-button"
              aria-label={`Copy base URL for ${a.label}`}
              onClick={() => void copy(a.value)}
            >
              <Copy size={14} />
            </button>
          </div>
        </div>
      ))}
      <p className="help-note">
        Anthropic clients such as Claude Code add /v1/messages themselves, so
        they need the address without /v1. In Delta, choose the Responses API
        with the /v1 address. Open a model to test the connection with your key.
      </p>
      <ul className="endpoint-list">
        {endpoints.map((e) => (
          <li key={e.path}>
            <span className="endpoint-method">{e.method}</span>
            <code>/v1{e.path}</code>
            <span className="endpoint-note">{e.note}</span>
            <button
              className="icon-button"
              aria-label={`Copy ${e.path.slice(1)} URL`}
              onClick={() => void copy(base + e.path)}
            >
              <Copy size={14} />
            </button>
          </li>
        ))}
      </ul>
      <div className="protocol-choice">
        <span>Example request</span>
        <Select
          label="Provider for example"
          value={provider}
          options={[
            { value: "claude", label: "Claude" },
            { value: "codex", label: "Codex" },
          ]}
          onChange={setProvider}
        />
        <Select
          label="Client protocol"
          value={protocol}
          options={protocols}
          onChange={setProtocol}
        />
      </div>
      <CodeBlock
        code={curlExample(base, protocol, "MODEL_ID", provider)}
        method="POST"
        path={`/v1${protocolPath(protocol)}`}
        copy={copy}
      />
    </Modal>
  );
}
