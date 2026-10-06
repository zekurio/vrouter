import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { Check, ChevronDown } from "lucide-react";

export type SelectOption<T extends string> = { value: T; label: string };

// Select-only combobox: focus stays on the trigger and the highlighted option
// is tracked with aria-activedescendant.
export function Select<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: T;
  options: SelectOption<T>[];
  onChange: (value: T) => void;
}) {
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const typed = useRef({ text: "", at: 0 });
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const selected = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );

  useEffect(() => {
    if (!open) return;
    const close = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", close);
    return () => document.removeEventListener("pointerdown", close);
  }, [open]);

  useEffect(() => {
    if (open)
      list.current?.children[active]?.scrollIntoView({ block: "nearest" });
  }, [open, active]);

  const show = () => {
    setActive(selected);
    setOpen(true);
  };
  const choose = (index: number) => {
    setOpen(false);
    if (options[index].value !== value) onChange(options[index].value);
  };
  const match = (key: string) => {
    const now = Date.now();
    const text = (now - typed.current.at > 600 ? "" : typed.current.text) + key;
    typed.current = { text, at: now };
    return options.findIndex((o) =>
      o.label.toLowerCase().startsWith(text.toLowerCase()),
    );
  };

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    if (e.key.length === 1 && e.key !== " ") {
      const index = match(e.key);
      if (index < 0) return;
      if (open) setActive(index);
      else choose(index);
      return;
    }
    if (!open) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        show();
      }
      return;
    }
    const last = options.length - 1;
    const move: Record<string, number> = {
      ArrowDown: Math.min(active + 1, last),
      ArrowUp: Math.max(active - 1, 0),
      Home: 0,
      End: last,
    };
    if (e.key in move) {
      e.preventDefault();
      setActive(move[e.key]);
    } else if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      choose(active);
    } else if (e.key === "Escape") {
      e.preventDefault();
      setOpen(false);
    }
  };

  return (
    <div
      className="select"
      ref={root}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget)) setOpen(false);
      }}
    >
      <button
        type="button"
        role="combobox"
        className="select-trigger"
        aria-label={label}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open ? `${id}-${active}` : undefined}
        onClick={(e) => {
          // Safari does not focus a button on click.
          e.currentTarget.focus();
          if (open) setOpen(false);
          else show();
        }}
        onKeyDown={onKeyDown}
      >
        {/* Every label shares one grid cell so the width fits the longest. */}
        <span className="select-value">
          {options.map((o) => (
            <span
              key={o.value}
              aria-hidden={o.value !== value}
              data-current={o.value === value || undefined}
            >
              {o.label}
            </span>
          ))}
        </span>
        <ChevronDown size={14} aria-hidden />
      </button>
      {open && (
        <ul
          className="select-menu"
          id={`${id}-list`}
          role="listbox"
          aria-label={label}
          ref={list}
          // Keep focus on the trigger while the pointer works the list.
          onMouseDown={(e) => e.preventDefault()}
        >
          {options.map((o, i) => (
            <li
              key={o.value}
              id={`${id}-${i}`}
              role="option"
              aria-selected={o.value === value}
              className={i === active ? "active" : ""}
              onMouseMove={() => setActive(i)}
              onClick={() => choose(i)}
            >
              {o.label}
              {o.value === value && <Check size={13} aria-hidden />}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
