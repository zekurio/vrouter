import { useEffect, useRef, useState, type PointerEvent } from "react";
import { providerLabel, providerStyle } from "./ProviderBrand";
import {
  compact,
  dayLabel,
  hourLabel,
  intervals,
  money,
  monthLabel,
  type Bucket,
  type Group,
  type Interval,
  type Usage,
} from "./usage";

// Chart series follow the provider, so a provider keeps its color in every
// view.
export const series = providerStyle;

const SIZE = { height: 210, left: 46, right: 10, top: 10, bottom: 24 };

// A round step that splits the range into about three intervals.
function tickStep(max: number) {
  const rough = max / 3;
  const power = 10 ** Math.floor(Math.log10(rough));
  const unit = rough / power;
  return (unit <= 1 ? 1 : unit <= 2 ? 2 : unit <= 5 ? 5 : 10) * power;
}
const axisMoney = (value: number) =>
  value === 0
    ? "0"
    : value < 1
      ? `$${value.toFixed(value < 0.1 ? 3 : 2)}`
      : `$${compact(value)}`;

const DAY = 24 * 3600e3;
const intervalLabel: Record<Interval, string> = {
  hour: "Hourly",
  day: "Daily",
  week: "Weekly",
  month: "Monthly",
};
const axisLabel: Record<Interval, (time: number) => string> = {
  hour: hourLabel,
  day: dayLabel,
  week: dayLabel,
  month: monthLabel,
};
const tipLabel: Record<Interval, (time: number) => string> = {
  hour: (time) => `${dayLabel(time)}, ${hourLabel(time)}`,
  day: dayLabel,
  // Noon of the last day, so a daylight-saving shift cannot move the date.
  week: (time) => `${dayLabel(time)} – ${dayLabel(time + 6.5 * DAY)}`,
  month: monthLabel,
};

// A bar segment. Only the end of a stack is rounded.
function segment(
  {
    x,
    y,
    width: w,
    height: h,
  }: {
    x: number;
    y: number;
    width: number;
    height: number;
  },
  radius: number,
) {
  const r = Math.min(radius, w / 2, h);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

// Where the buckets and costs land in a chart of the given width.
function chartScale(buckets: Bucket[], providers: Group[], width: number) {
  const sum = (b: Bucket) =>
    providers.reduce((n, p) => n + (b.byProvider[p.name] ?? 0), 0);
  const peak = Math.max(...buckets.map((b) => sum(b)), 0);
  const step = tickStep(peak || 1);
  const top = Math.ceil((peak || 1) / step) * step;
  const ticks = Array.from(
    { length: Math.round(top / step) + 1 },
    (_, i) => i * step,
  );
  const inner = Math.max(width - SIZE.left - SIZE.right, 0);
  const plot = SIZE.height - SIZE.top - SIZE.bottom;
  const band = inner / buckets.length;
  const bar = Math.min(Math.max(band - 2, 1), 40);
  const x = (i: number) => SIZE.left + band * (i + 0.5);
  const y = (value: number) => SIZE.top + plot * (1 - value / top);
  return { sum, ticks, plot, band, bar, x, y, width };
}
type Scale = ReturnType<typeof chartScale>;

// Tracks the width of the element the ref is attached to.
function useWidth() {
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

export function CostChart({ usage }: { usage: Usage }) {
  const { frame, width } = useWidth();
  const [hover, setHover] = useState<number | null>(null);
  const [chosen, setChosen] = useState<Interval | null>(null);

  const interval = chosen ?? (usage.recent ? "hour" : "day");
  const buckets = usage.series[interval];
  const providers = usage.providers.filter((p) => p.cost > 0);
  const scale = chartScale(buckets, providers, width);
  const { band } = scale;

  function track(event: PointerEvent<SVGSVGElement>) {
    const left = event.currentTarget.getBoundingClientRect().left;
    const index = Math.floor((event.clientX - left - SIZE.left) / (band || 1));
    setHover(Math.min(Math.max(index, 0), buckets.length - 1));
  }
  const at: Bucket | null = hover === null ? null : (buckets[hover] ?? null);

  return (
    <figure className="usage-chart">
      <figcaption>
        Cost
        <div className="tabs" role="group" aria-label="Chart interval">
          {intervals.map((value) => (
            <button
              key={value}
              aria-pressed={interval === value}
              className={interval === value ? "selected" : ""}
              onClick={() => {
                setHover(null);
                setChosen(value);
              }}
            >
              {intervalLabel[value]}
            </button>
          ))}
        </div>
      </figcaption>
      <div ref={frame}>
        {width > 0 && (
          <svg
            width={width}
            height={SIZE.height}
            role="img"
            aria-label={`${intervalLabel[interval]} cost by provider. The Day breakdown below lists daily values.`}
            onPointerMove={track}
            onPointerLeave={() => setHover(null)}
          >
            {at && hover !== null && (
              <rect
                className="band"
                x={SIZE.left + band * hover}
                y={SIZE.top}
                width={band}
                height={scale.plot}
              />
            )}
            <ChartAxes scale={scale} buckets={buckets} interval={interval} />
            <ChartBars scale={scale} buckets={buckets} providers={providers} />
          </svg>
        )}
        {at && hover !== null && (
          <ChartTip
            scale={scale}
            hover={hover}
            at={at}
            interval={interval}
            providers={providers}
          />
        )}
      </div>
    </figure>
  );
}

// Where the label of bucket i sits: the first and last labels line up with the
// ends of the plot.
function markX(i: number, count: number, scale: Scale) {
  if (count === 1) return scale.x(i);
  if (i === 0) return SIZE.left;
  if (i === count - 1) return scale.width - SIZE.right;
  return scale.x(i);
}
function markAnchor(i: number, count: number) {
  if (count > 1 && i === 0) return "start";
  if (count > 1 && i === count - 1) return "end";
  return "middle";
}

// Grid lines with cost labels, and time labels under the plot.
function ChartAxes({
  scale,
  buckets,
  interval,
}: {
  scale: Scale;
  buckets: Bucket[];
  interval: Interval;
}) {
  const { y, width } = scale;
  // Axis labels at the first, middle and last bucket.
  const marks = [
    ...new Set([0, Math.floor((buckets.length - 1) / 2), buckets.length - 1]),
  ].flatMap((i) => {
    const b = buckets[i];
    return b ? [{ i, start: b.start }] : [];
  });
  return (
    <>
      {scale.ticks.map((tick) => (
        <g key={tick}>
          <line
            className="grid"
            x1={SIZE.left}
            x2={width - SIZE.right}
            y1={y(tick)}
            y2={y(tick)}
          />
          <text x={SIZE.left - 10} y={y(tick) + 3.5} textAnchor="end">
            {axisMoney(tick)}
          </text>
        </g>
      ))}
      {marks.map(({ i, start }) => (
        <text
          key={i}
          x={markX(i, buckets.length, scale)}
          y={SIZE.height - 5}
          textAnchor={markAnchor(i, buckets.length)}
        >
          {axisLabel[interval](start)}
        </text>
      ))}
    </>
  );
}

// One stack of provider segments per bucket.
function ChartBars({
  scale,
  buckets,
  providers,
}: {
  scale: Scale;
  buckets: Bucket[];
  providers: Group[];
}) {
  const { x, y, bar } = scale;
  const base = y(0);
  return buckets.map((b, i) => {
    const parts = providers.filter((p) => (b.byProvider[p.name] ?? 0) > 0);
    let floor = base;
    return parts.map((p, n) => {
      // Any cost shows, and stacked segments keep a gap between them.
      const height = Math.max(base - y(b.byProvider[p.name] ?? 0), 2);
      const gap = n > 0 && height > 3 ? 2 : 0;
      floor -= height;
      return (
        <path
          key={`${b.start}/${p.name}`}
          className="bar"
          style={series(p.provider)}
          d={segment(
            { x: x(i) - bar / 2, y: floor, width: bar, height: height - gap },
            n === parts.length - 1 ? 4 : 0,
          )}
        />
      );
    });
  });
}

function ChartTip({
  scale,
  hover,
  at,
  interval,
  providers,
}: {
  scale: Scale;
  hover: number;
  at: Bucket;
  interval: Interval;
  providers: Group[];
}) {
  const { x } = scale;
  return (
    <div
      className={`chart-tip ${x(hover) > scale.width / 2 ? "left" : ""}`}
      style={{ left: x(hover) }}
      role="status"
    >
      <p>{tipLabel[interval](at.start)}</p>
      {providers.map((p) => (
        <div key={p.name} style={series(p.provider)}>
          <span className="series-key" aria-hidden="true" />
          <strong>{money(at.byProvider[p.name] ?? 0)}</strong>
          {providerLabel(p.provider)}
        </div>
      ))}
      {providers.length > 1 && (
        <div>
          <strong>{money(scale.sum(at))}</strong>
          Total
        </div>
      )}
    </div>
  );
}
