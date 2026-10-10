import { createContext, useContext, useState } from "react";
import { readStored, writeStored } from "./storage";

const storageKey = "vrouter-hide-emails";
const email = /[^\s@<>()]+@[^\s@<>()]+\.[^\s@<>()]+/gu;
// Shown blurred in place of an address, so the real one never reaches the page.
const standIn = "hidden@email.address";

// Hidden unless the user has explicitly chosen "visible", including when
// storage is unavailable.
export const storedHideEmails = () => readStored(storageKey) !== "visible";
export const storeHideEmails = (hidden: boolean) =>
  writeStored(storageKey, hidden ? "hidden" : "visible");

const HideEmails = createContext(false);
export const PrivacyProvider = HideEmails.Provider;

// Returns text for labels and tooltips. A value holding an address becomes the fallback.
export function usePrivateLabel() {
  const hidden = useContext(HideEmails);
  return (value: string, fallback: string) =>
    hidden && value.search(email) >= 0 ? fallback : value;
}

// One masked address. With `peek`, hovering or focusing it shows the real
// address in a floating layer that leaves the stand-in in the flow, so the row
// never changes size. The real text is only in the DOM while revealed, and the
// shared hide setting is untouched.
function Masked({ address, peek }: { address: string; peek: boolean }) {
  const [hover, setHover] = useState(false);
  const [focus, setFocus] = useState(false);
  const stand = (
    <>
      <span className="private-text" aria-hidden="true">
        {standIn}
      </span>
      <span className="sr-only">hidden email</span>
    </>
  );
  if (!peek) return stand;
  const shown = hover || focus;
  return (
    <button
      type="button"
      className={`private-peek ${shown ? "is-shown" : ""}`}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      onFocus={() => setFocus(true)}
      onBlur={() => setFocus(false)}
      // Safari and touch browsers do not focus a button on click.
      onClick={(e) => e.currentTarget.focus()}
    >
      {stand}
      {shown && <span className="private-reveal">{address}</span>}
    </button>
  );
}

export function Private({
  children,
  peek = false,
}: {
  children: string;
  // Let the viewer reveal each hidden address on hover or keyboard focus.
  peek?: boolean;
}) {
  const hidden = useContext(HideEmails);
  const found = hidden ? [...children.matchAll(email)] : [];
  if (found.length === 0) return children;
  // Each address with the text before it, keyed by where the address starts.
  let last = 0;
  const parts = found.map((m) => {
    const before = children.slice(last, m.index);
    last = m.index + m[0].length;
    return { at: m.index, before, address: m[0] };
  });
  return (
    <span>
      {parts.map((part) => (
        <span key={part.at}>
          {part.before}
          <Masked address={part.address} peek={peek} />
        </span>
      ))}
      {children.slice(last)}
    </span>
  );
}
