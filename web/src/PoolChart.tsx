import { useState, type PointerEvent } from "react";
import { axisText, ChartTip, tipRow, tipValue, useWidth } from "./chart";
import {
  DAY,
  HOUR,
  poolValue,
  type Basis,
  type Point,
  type PoolForecast,
} from "./poolForecast";
import { hourLabel } from "./usage";

const SIZE = { height: 190, left: 44, right: 12, top: 26, bottom: 24 };
// Rises smaller than this get no label.
const labelledGain = 1;

const weekday = (time: number) =>
  new Date(time).toLocaleDateString(undefined, {
    weekday: "short",
    day: "numeric",
  });
const moment = (time: number) =>
  new Date(time).toLocaleString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });

function chartScale(f: PoolForecast, width: number) {
  const inner = Math.max(width - SIZE.left - SIZE.right, 1);
  const plot = SIZE.height - SIZE.top - SIZE.bottom;
  const x = (t: number) => SIZE.left + ((t - f.from) / (f.to - f.from)) * inner;
  const y = (v: number) => SIZE.top + plot * (1 - v / f.top);
  const time = (px: number) =>
    f.from + ((px - SIZE.left) / inner) * (f.to - f.from);
  return { x, y, time, inner, plot, width };
}
type Scale = ReturnType<typeof chartScale>;

const linePath = (points: Point[], s: Scale) =>
  points
    .map(
      (p, i) => `${i ? "L" : "M"}${s.x(p.t).toFixed(1)},${s.y(p.v).toFixed(1)}`,
    )
    .join("");
function areaPath(points: Point[], s: Scale) {
  const first = points[0];
  if (!first || points.length < 2) return "";
  return `${linePath(points, s)}V${s.y(0)}H${s.x(first.t)}Z`;
}

// Day boundaries, with the days that fit labelled at noon. Spans of two days
// or less get hours instead.
function timeTicks(f: PoolForecast, s: Scale) {
  const span = f.to - f.from;
  const perHour = (s.inner / span) * HOUR;
  const lines: number[] = [];
  const labels: { t: number; text: string }[] = [];
  const at = new Date(f.from);
  if (span > 2 * DAY) {
    const every = Math.max(1, Math.ceil(64 / (perHour * 24)));
    at.setHours(0, 0, 0, 0);
    while (at.getTime() <= f.to) {
      const start = at.getTime();
      if (start > f.from) lines.push(start);
      at.setHours(12);
      const noon = at.getTime();
      if (noon > f.from && noon < f.to && Math.round(start / DAY) % every === 0)
        labels.push({ t: noon, text: weekday(start) });
      at.setDate(at.getDate() + 1);
      at.setHours(0);
    }
  } else {
    const every = [1, 2, 3, 6, 12].find((h) => h * perHour >= 56) ?? 24;
    at.setMinutes(0, 0, 0);
    while (at.getTime() <= f.to) {
      const t = at.getTime();
      if (t > f.from && at.getHours() % every === 0) {
        lines.push(t);
        labels.push({ t, text: hourLabel(t) });
      }
      at.setHours(at.getHours() + 1);
    }
  }
  return { lines, labels };
}

// The +n% labels on rises that have room, away from the Now label.
function jumpLabels(f: PoolForecast, s: Scale) {
  let last = -Infinity;
  return f.jumps.filter((j) => {
    const x = s.x(j.t);
    if (j.gain < labelledGain || j.t < f.from || j.t > f.to) return false;
    if (x - last < 34 || Math.abs(x - s.x(f.now)) < 24) return false;
    last = x;
    return true;
  });
}

export function PoolChart({
  forecast: f,
  label,
  basis,
}: {
  forecast: PoolForecast;
  // The provider, for the chart's accessible name.
  label: string;
  // The basis the user chose. The forecast says which one it could use.
  basis: Basis;
}) {
  const { frame, width } = useWidth();
  const [hover, setHover] = useState<number | null>(null);
  const s = chartScale(f, width);
  const current = f.projected[0]?.v ?? 0;

  function track(event: PointerEvent<SVGSVGElement>) {
    const left = event.currentTarget.getBoundingClientRect().left;
    const px = Math.min(
      Math.max(event.clientX - left, SIZE.left),
      width - SIZE.right,
    );
    setHover(s.time(px));
  }

  return (
    // Above the pool's filter button, which covers the rest of the panel.
    <figure className="relative z-1 mt-5 min-w-0">
      <div ref={frame} className="relative">
        {width > 0 && (
          <svg
            className="touch-pan-y overflow-visible"
            width={width}
            height={SIZE.height}
            role="img"
            aria-label={`${label} allowance over time: recorded, and projected for the next window.`}
            onPointerMove={track}
            onPointerLeave={() => setHover(null)}
          >
            <ChartGrid forecast={f} scale={s} />
            <ChartLines forecast={f} scale={s} current={current} />
            {hover !== null && <Crosshair forecast={f} scale={s} at={hover} />}
          </svg>
        )}
        {hover !== null && <PoolTip forecast={f} scale={s} at={hover} />}
      </div>
      <figcaption className="mt-2.5 flex flex-wrap items-center gap-x-4.5 gap-y-1 text-[12px] text-muted">
        {f.recorded.length > 1 && (
          <span className="flex items-center gap-1.75">
            <i className={recordedKey} aria-hidden="true" />
            Recorded
          </span>
        )}
        <span className="flex items-center gap-1.75">
          <i className={projectedKey} aria-hidden="true" />
          Projected from {basisPhrase[f.basis]}
          {f.basis !== basis &&
            `, as there is too little history for ${basisPhrase[basis]}`}
        </span>
      </figcaption>
    </figure>
  );
}

const recordedKey = "h-0.5 w-3.5 rounded-full bg-provider";
const projectedKey = "w-3.5 border-t-2 border-dashed border-provider";
const basisPhrase: Record<Basis, string> = {
  "3h": "the last 3 hours",
  day: "the last day",
  window: "the pace since the reset",
};

// Gridlines with share labels, day or hour boundaries, and the Now line.
function ChartGrid({
  forecast: f,
  scale: s,
}: {
  forecast: PoolForecast;
  scale: Scale;
}) {
  const { lines, labels } = timeTicks(f, s);
  const bottom = SIZE.top + s.plot;
  return (
    <>
      {[0, f.top / 2, f.top].map((v) => (
        <g key={v}>
          <line
            className="stroke-border"
            x1={SIZE.left}
            x2={s.width - SIZE.right}
            y1={s.y(v)}
            y2={s.y(v)}
          />
          <text
            className={axisText}
            x={SIZE.left - 10}
            y={s.y(v) + 3.5}
            textAnchor="end"
          >
            {Math.round(v)}%
          </text>
        </g>
      ))}
      {lines.map((t) => (
        <line
          key={t}
          className="stroke-border"
          x1={s.x(t)}
          x2={s.x(t)}
          y1={SIZE.top}
          y2={bottom}
        />
      ))}
      {labels.map(({ t, text }) => (
        <text
          key={t}
          className={axisText}
          x={s.x(t)}
          y={SIZE.height - 5}
          textAnchor="middle"
        >
          {text}
        </text>
      ))}
      <line
        className="stroke-muted"
        strokeOpacity={0.5}
        x1={s.x(f.now)}
        x2={s.x(f.now)}
        y1={SIZE.top - 6}
        y2={bottom}
      />
      <text
        className={axisText}
        x={s.x(f.now)}
        y={SIZE.top - 11}
        textAnchor="middle"
      >
        Now
      </text>
    </>
  );
}

// The recorded line and its wash, the dashed projection, the rises, where
// the pool runs dry, and the current share.
function ChartLines({
  forecast: f,
  scale: s,
  current,
}: {
  forecast: PoolForecast;
  scale: Scale;
  current: number;
}) {
  const stroke =
    "fill-none stroke-provider [stroke-linecap:round] [stroke-linejoin:round]";
  return (
    <>
      <path className="fill-provider/10" d={areaPath(f.recorded, s)} />
      <path className="fill-provider/4" d={areaPath(f.projected, s)} />
      <path className={stroke} strokeWidth={2} d={linePath(f.recorded, s)} />
      <path
        className={stroke}
        strokeWidth={2}
        strokeDasharray="3 5"
        d={linePath(f.projected, s)}
      />
      {jumpLabels(f, s).map((j) => (
        <text
          key={j.t}
          className="fill-muted text-[10.5px] tabular-nums"
          x={s.x(j.t)}
          y={Math.max(s.y(j.v) - 7, 10)}
          textAnchor="middle"
        >
          +{Math.round(j.gain)}%
        </text>
      ))}
      {/* The stats name the time, and the tooltip on the marker. */}
      {f.empty !== null && f.empty > f.now && (
        <circle
          className="fill-surface stroke-provider"
          strokeWidth={2}
          cx={s.x(f.empty)}
          cy={s.y(0)}
          r={4}
        />
      )}
      <circle
        className="fill-provider stroke-surface"
        strokeWidth={2}
        cx={s.x(f.now)}
        cy={s.y(current)}
        r={4}
      />
    </>
  );
}

function Crosshair({
  forecast: f,
  scale: s,
  at,
}: {
  forecast: PoolForecast;
  scale: Scale;
  at: number;
}) {
  const value = poolValue(f, at);
  return (
    <>
      <line
        className="stroke-muted"
        x1={s.x(at)}
        x2={s.x(at)}
        y1={SIZE.top}
        y2={SIZE.top + s.plot}
      />
      {value !== null && (
        <circle
          className="fill-provider stroke-surface"
          strokeWidth={2}
          cx={s.x(at)}
          cy={s.y(value)}
          r={4}
        />
      )}
    </>
  );
}

function PoolTip({
  forecast: f,
  scale: s,
  at,
}: {
  forecast: PoolForecast;
  scale: Scale;
  at: number;
}) {
  const value = poolValue(f, at);
  const past = at <= f.now;
  return (
    <ChartTip x={s.x(at)} width={s.width}>
      <p>{moment(at)}</p>
      <div className={tipRow}>
        <i className={past ? recordedKey : projectedKey} aria-hidden="true" />
        {value === null ? (
          "Not recorded"
        ) : (
          <>
            <strong className={tipValue}>{Math.round(value)}%</strong>
            {past ? "Recorded" : "Projected"}
          </>
        )}
      </div>
    </ChartTip>
  );
}
