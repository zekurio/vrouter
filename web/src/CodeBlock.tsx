import { useEffect, useState, type ReactNode } from "react";
import { Check, Copy } from "lucide-react";

// Enough of shell and JSON to colour the curl examples; anything else stays plain.
const token =
  /(https?:\/\/[^\s'"]+)|("(?:[^"\\\n]|\\.)*")(\s*:)?|(\$[A-Za-z_]\w*)|((?<=\s)--?[A-Za-z][\w-]*)|(\b(?:true|false|null)\b|(?<![\w.-])-?\d+(?:\.\d+)?(?![\w.-]))|(\\$)|(^curl\b)/gm;
const variable = /(\$[A-Za-z_]\w*)/;

function highlight(code: string) {
  const out: ReactNode[] = [];
  let last = 0;
  const push = (kind: string, text: ReactNode) =>
    out.push(
      <span key={out.length} className={`tok-${kind}`}>
        {text}
      </span>,
    );
  for (const m of code.matchAll(token)) {
    if (m.index > last) out.push(code.slice(last, m.index));
    last = m.index + m[0].length;
    if (m[1]) push("url", m[1]);
    else if (m[2] && m[3]) {
      push("key", m[2]);
      out.push(m[3]);
    } else if (m[2])
      push(
        "string",
        m[2].split(variable).map((part, i) =>
          i % 2 ? (
            <span key={i} className="tok-var">
              {part}
            </span>
          ) : (
            part
          ),
        ),
      );
    else if (m[4]) push("var", m[4]);
    else if (m[5]) push("flag", m[5]);
    else if (m[6]) push("literal", m[6]);
    else if (m[7]) push("escape", m[7]);
    else push("command", m[0]);
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
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1800);
    return () => clearTimeout(timer);
  }, [copied]);

  return (
    <div className="code-block">
      <div className="code-block-bar">
        <span className="endpoint-method">{method}</span>
        <code>{path}</code>
        <button
          className="icon-button"
          aria-label="Copy request"
          onClick={async () => {
            if (await copy(code)) setCopied(true);
          }}
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </div>
      <pre tabIndex={0}>{highlight(code)}</pre>
    </div>
  );
}
