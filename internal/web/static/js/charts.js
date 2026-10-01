// Time-series charts (uPlot). Colors are read from the theme's CSS
// variables, so charts follow the light/dark theme.

import { html, setHTML } from "./util.js";

export function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export const SERIES_COLORS = ["--chart-1", "--chart-2", "--chart-3", "--chart-4", "--chart-5", "--chart-6"];

// lineChart(el, {series: [{label, color}], max, fmt}) draws into el and
// returns {update(xs, ...ys), destroy()}. xs are unix seconds.
export function lineChart(el, { series, max = null, fmt = (v) => String(v), height = 180, axisWidth = 56, minTop = 1 }) {
  const textDim = cssVar("--text-dim");
  const grid = cssVar("--chart-grid");
  const colors = series.map((s, i) => cssVar(s.color || SERIES_COLORS[i % SERIES_COLORS.length]));

  setHTML(el, html`<div class="chart"></div><div class="chart-legend">${series.map((s, i) =>
    html`<span><i style="background:${colors[i]}"></i>${s.label}</span>`)}</div>`);
  const target = el.querySelector(".chart");

  const axis = { stroke: textDim, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 }, font: "11px system-ui" };
  const opts = {
    width: Math.max(200, target.clientWidth),
    height,
    cursor: { y: false },
    legend: { show: false },
    scales: { x: { time: true }, y: max === null ? { range: (u, lo, hi) => [0, Math.max(hi * 1.1, minTop)] } : { range: [0, max] } },
    axes: [axis, { ...axis, size: axisWidth, values: (u, vals) => vals.map(fmt) }],
    series: [{}, ...series.map((s, i) => ({
      label: s.label, stroke: colors[i], width: 1.6, points: { show: false },
      fill: series.length === 1 ? colors[i] + "22" : undefined,
    }))],
  };
  const empty = [[], ...series.map(() => [])];
  const plot = new uPlot(opts, empty, target);

  const ro = new ResizeObserver(() => plot.setSize({ width: Math.max(200, target.clientWidth), height }));
  ro.observe(target);
  return {
    update(xs, ...ys) { plot.setData([xs, ...ys]); },
    destroy() { ro.disconnect(); plot.destroy(); },
  };
}
