import { useState } from "react";
import { Copy, X } from "lucide-react";
import { CodeBlock, endpointMethod, protocolChoice } from "./CodeBlock";
import { Modal } from "./Modal";
import {
  curlExample,
  protocolPath,
  protocols,
  type Protocol,
} from "./protocols";
import { Select } from "./Select";
import { copyField, dialogHeading, dialogTitle, iconButton } from "./ui";

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
  const origin = publicUrl || location.origin;
  const base = `${origin}/v1`;
  const addresses = [
    { label: "OpenAI-compatible clients", value: base },
    { label: "Anthropic clients", value: origin },
  ];
  return (
    <Modal labelledBy="help-title" onClose={onClose}>
      <div className={`${dialogHeading} mb-5`}>
        <h2 id="help-title" className={dialogTitle}>
          Client API
        </h2>
        <button
          className={iconButton()}
          aria-label="Close help"
          onClick={onClose}
        >
          <X size={18} />
        </button>
      </div>
      {addresses.map((a, i) => (
        <div className={i > 0 ? "mt-2.5" : ""} key={a.label}>
          <span className="mb-1.5 block text-[11px] text-muted">{a.label}</span>
          <div className={copyField}>
            <code className="min-w-0 wrap-anywhere">{a.value}</code>
            <button
              className={iconButton()}
              aria-label={`Copy base URL for ${a.label}`}
              onClick={() => void copy(a.value)}
            >
              <Copy size={14} />
            </button>
          </div>
        </div>
      ))}
      <ul className="my-3.5">
        {endpoints.map((e) => (
          <li
            key={e.path}
            className="flex items-center gap-2.5 py-0.5 pl-3 text-[12px]"
          >
            <span className={`w-8.5 ${endpointMethod}`}>{e.method}</span>
            <code>/v1{e.path}</code>
            <span className="min-w-0 flex-1 text-right text-subtle">
              {e.note}
            </span>
            <button
              className={iconButton()}
              aria-label={`Copy ${e.path.slice(1)} URL`}
              onClick={() => void copy(base + e.path)}
            >
              <Copy size={14} />
            </button>
          </li>
        ))}
      </ul>
      <div className={protocolChoice}>
        <span>Example request</span>
        <Select
          inset
          label="Client protocol"
          value={protocol}
          options={protocols}
          onChange={setProtocol}
        />
      </div>
      <CodeBlock
        code={curlExample(base, protocol, "MODEL_ID")}
        method="POST"
        path={`/v1${protocolPath(protocol)}`}
        copy={copy}
      />
    </Modal>
  );
}
