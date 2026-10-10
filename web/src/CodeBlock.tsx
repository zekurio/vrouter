import { useEffect, useState, type ReactNode } from "react";
import { Check, Copy } from "lucide-react";

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
      <span key={m.index} className="tok-var">
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
      <span key={out.length} className={`tok-${kind}`}>
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
    <div className="code-block">
      <div className="code-block-bar">
        <span className="endpoint-method">{method}</span>
        <code>{path}</code>
        <button
          className="icon-button"
          aria-label="Copy request"
          onClick={() => void copyCode()}
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </div>
      <pre tabIndex={0}>{highlight(code)}</pre>
    </div>
  );
}
