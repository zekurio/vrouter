// Pieces the cost chart and the pool charts share.
import { useEffect, useRef, useState, type ReactNode } from "react";
import { cx } from "./ui";

export const axisText = "fill-muted text-[11px]";

// Tracks the width of the element the ref is attached to.
export function useWidth() {
  const frame = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const node = frame.current;
    if (!node) return undefined;
    const observer = new ResizeObserver(() => setWidth(node.clientWidth));
    observer.observe(node);
    return () => observer.disconnect();
  }, []);
  return { frame, width };
}

export const tipRow = "flex items-center gap-1.75";
export const tipValue = "font-[550] text-text tabular-nums";

// The hover readout, beside x on the side of the chart with more room.
export function ChartTip({
  x,
  width,
  children,
}: {
  x: number;
  width: number;
  children: ReactNode;
}) {
  return (
    <div
      className={cx(
        "pointer-events-none absolute top-1.5 z-2 grid gap-1.5 rounded-[10px] border border-line bg-control px-3 py-2.5 text-[12px] whitespace-nowrap text-muted shadow-[0_10px_30px_#0006]",
        x > width / 2 ? "-translate-x-[calc(100%+12px)]" : "translate-x-3",
      )}
      style={{ left: x }}
      role="status"
    >
      {children}
    </div>
  );
}
