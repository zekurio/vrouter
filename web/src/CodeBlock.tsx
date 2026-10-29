import { useEffect, useState, type ReactNode } from "react";
import { Check, Copy } from "lucide-react";
import { iconButton } from "./ui";

// The HTTP method beside a path.
export const endpointMethod =
  "text-[10px] font-semibold tracking-[0.4px] text-muted";
// The label and protocol switch above an example request.
export const protocolChoice =
  "mb-2 flex items-center justify-between gap-2.5 text-[11px] text-muted";

const tokenColors: Record<string, string> = {
  command: "font-semibold text-text",
  url: "text-text",
  key: "text-text",
  flag: "text-accent",
  string: "text-green",
  var: "text-amber",
  literal: "text-amber",
  escape: "text-muted",
};

// Enough of shell and JSON to colour the curl examples; anything else stays plain.
const token =
  /(https?:\/\/[^\s'"]+)|("(?:[^"\\\n]|\\.)*")(\s*:)?|(\$[A-Za-z_]\w*)|((?<=\s)--?[A-Za-z][\w-]*)|(\b(?:true|false|null)\b|(?<![\w.-])-?\d+(?:\.\d+)?(?![\w.-]))|(\\$)|(^curl\b)/gmu;
const variable = /\$[A-Za-z_]\w*/gu;

// A string with each $VARIABLE in it marked, keyed by its offset.
function withVariables(text: string) {
  const out: ReactNode[] = [];
  let last = 0;
  for (const m of text.matchAll(variable)) {
    out.push(
      text.slice(last, m.index),
      <span key={m.index} className={tokenColors["var"]}>
        {m[0]}
      </span>,
    );
    last = m.index + m[0].length;
  }
  out.push(text.slice(last));
  return out;
}

function highlight(code: string) {
  const out: ReactNode[] = [];
  let last = 0;
  const push = (kind: string, text: ReactNode) =>
    out.push(
      <span key={out.length} className={tokenColors[kind]}>
        {text}
      </span>,
    );
  // Every group that took part in a match is non-empty.
  for (const m of code.matchAll(token)) {
    if (m.index > last) out.push(code.slice(last, m.index));
    last = m.index + m[0].length;
    if (m[1] !== undefined) push("url", m[1]);
    else if (m[2] !== undefined && m[3] !== undefined) {
      push("key", m[2]);
      out.push(m[3]);
    } else if (m[2] !== undefined) push("string", withVariables(m[2]));
    else if (m[4] !== undefined) push("var", m[4]);
    else if (m[5] !== undefined) push("flag", m[5]);
    else if (m[6] !== undefined) push("literal", m[6]);
    else if (m[7] === undefined) push("command", m[0]);
    else push("escape", m[7]);
  }
  out.push(code.slice(last));
  return out;
}

export function CodeBlock({
  code,
  method,
  path,
  copy,
}: {
  code: string;
  method: string;
  path: string;
  copy: (value: string) => Promise<boolean>;
}) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(false), 1800);
    return () => clearTimeout(timer);
  }, [copied]);

  const copyCode = async () => {
    if (await copy(code)) setCopied(true);
  };

  return (
    <div className="overflow-hidden rounded-lg border border-line bg-well">
      <div className="flex items-center gap-2.5 border-b border-border py-0.75 pr-0.75 pl-3.5">
        <span className={endpointMethod}>{method}</span>
        <code className="min-w-0 flex-1 truncate text-subtle">{path}</code>
        <button
          className={iconButton()}
          aria-label="Copy request"
          onClick={() => void copyCode()}
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </div>
      <pre
        className="overflow-x-auto px-4 pt-3.5 pb-4 text-[12px] leading-[1.7] text-subtle [scrollbar-color:var(--color-border)_transparent] [scrollbar-width:thin] [tab-size:2] focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent"
        tabIndex={0}
      >
        {highlight(code)}
      </pre>
    </div>
  );
}
