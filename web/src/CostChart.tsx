import { useState, type PointerEvent } from "react";
import { axisText, ChartTip, tipRow, tipValue, useWidth } from "./chart";
import { providerLabel, providerStyle } from "./ProviderBrand";
import {
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
import { tab, tabs } from "./ui";

// Chart series follow the provider, so a provider keeps its color in every
// view.
export const series = providerStyle;
// The dot that names a series, in its provider's colour.
export const seriesKey = "size-2 rounded-full bg-provider";

// The top margin leaves room for the label over the tallest bar.
const SIZE = { height: 210, left: 46, right: 10, top: 22, bottom: 24 };
// The thickest a bar gets, so a short range does not draw blocks.
const MAX_BAR = 24;
// The least room an axis label gets, by interval.
const labelRoom: Record<Interval, number> = {
  hour: 52,
  day: 60,
  week: 60,
  month: 76,
};

// A round step that splits the range into about three intervals.
function tickStep(max: number) {
  const rough = max / 3;
  const power = 10 ** Math.floor(Math.log10(rough));
  const unit = rough / power;
  return (unit <= 1 ? 1 : unit <= 2 ? 2 : unit <= 5 ? 5 : 10) * power;
}
// Short amounts in the reader's currency format, as the totals use.
const shortMoney = (value: number) =>
  value.toLocaleString(undefined, {
    style: "currency",
    currency: "USD",
    notation: "compact",
    maximumSignificantDigits: value < 1 ? 2 : 3,
  });

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
  const totals = buckets.map((b) => sum(b));
  const peak = Math.max(...totals, 0);
  const step = tickStep(peak || 1);
  const top = Math.ceil((peak || 1) / step) * step;
  const ticks = Array.from(
    { length: Math.round(top / step) + 1 },
    (_, i) => i * step,
  );
  const inner = Math.max(width - SIZE.left - SIZE.right, 0);
  const plot = SIZE.height - SIZE.top - SIZE.bottom;
  const band = inner / buckets.length;
  const bar = Math.min(Math.max(band - 2, 1), MAX_BAR);
  const x = (i: number) => SIZE.left + band * (i + 0.5);
  const y = (value: number) => SIZE.top + plot * (1 - value / top);
  return {
    sum,
    peak,
    peakAt: totals.indexOf(peak),
    ticks,
    plot,
    band,
    bar,
    x,
    y,
    width,
  };
}
type Scale = ReturnType<typeof chartScale>;

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
    <figure className="min-w-0">
      <figcaption className="mb-3.5 flex items-center justify-between gap-3 text-[15px] font-semibold">
        Cost
        <div className={tabs(true)} role="group" aria-label="Chart interval">
          {intervals.map((value) => (
            <button
              key={value}
              aria-pressed={interval === value}
              className={tab(interval === value)}
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
      <div ref={frame} className="relative">
        {width > 0 && (
          <svg
            className="touch-pan-y overflow-visible"
            width={width}
            height={SIZE.height}
            role="img"
            aria-label={`${intervalLabel[interval]} cost by provider. The Day breakdown below lists daily values.`}
            onPointerMove={track}
            onPointerLeave={() => setHover(null)}
          >
            {at && hover !== null && (
              <rect
                className="fill-hover"
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
          <ChartTip x={scale.x(hover)} width={scale.width}>
            <CostTip
              scale={scale}
              at={at}
              interval={interval}
              providers={providers}
            />
          </ChartTip>
        )}
      </div>
    </figure>
  );
}

// The buckets that get a time label: every few, counted back from the
// newest, so the latest bucket is always named and labels keep their room.
function labelled(count: number, band: number, interval: Interval) {
  const every = Math.max(1, Math.ceil(labelRoom[interval] / (band || 1)));
  return Array.from({ length: count }, (_, i) => i).filter(
    (i) => (count - 1 - i) % every === 0,
  );
}

// Grid lines with cost labels, time labels under the plot, and the cost of
// the tallest bar over it.
function ChartAxes({
  scale,
  buckets,
  interval,
}: {
  scale: Scale;
  buckets: Bucket[];
  interval: Interval;
}) {
  const { x, y, width, peak, peakAt } = scale;
  return (
    <>
      {scale.ticks.map((tick) => (
        <g key={tick}>
          <line
            className="stroke-border"
            x1={SIZE.left}
            x2={width - SIZE.right}
            y1={y(tick)}
            y2={y(tick)}
          />
          <text
            className={axisText}
            x={SIZE.left - 10}
            y={y(tick) + 3.5}
            textAnchor="end"
          >
            {shortMoney(tick)}
          </text>
        </g>
      ))}
      {labelled(buckets.length, scale.band, interval).map((i) => (
        <text
          key={i}
          className={axisText}
          x={x(i)}
          y={SIZE.height - 5}
          textAnchor="middle"
        >
          {axisLabel[interval](buckets[i]?.start ?? 0)}
        </text>
      ))}
      {peak > 0 && (
        <text
          className="fill-subtle text-[11px] font-medium tabular-nums"
          x={x(peakAt)}
          y={y(peak) - 7}
          textAnchor="middle"
        >
          {shortMoney(peak)}
        </text>
      )}
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
          className="fill-provider"
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

// The bucket's period and requests, then each provider that cost something.
function CostTip({
  scale,
  at,
  interval,
  providers,
}: {
  scale: Scale;
  at: Bucket;
  interval: Interval;
  providers: Group[];
}) {
  const spent = providers.filter((p) => (at.byProvider[p.name] ?? 0) > 0);
  return (
    <>
      <p>
        {tipLabel[interval](at.start)}
        {at.requests > 0 &&
          ` · ${at.requests.toLocaleString()} ${at.requests === 1 ? "request" : "requests"}`}
      </p>
      {at.requests === 0 && <p className="text-text">No requests</p>}
      {spent.map((p) => (
        <div key={p.name} className={tipRow} style={series(p.provider)}>
          <span className={seriesKey} aria-hidden="true" />
          <strong className={tipValue}>
            {money(at.byProvider[p.name] ?? 0)}
          </strong>
          {providerLabel(p.provider)}
        </div>
      ))}
      {spent.length > 1 && (
        <div className={tipRow}>
          <strong className={tipValue}>{money(scale.sum(at))}</strong>
          Total
        </div>
      )}
    </>
  );
}
