// Dashboard: pool totals, node and task counts, cluster-wide usage charts
// broken down by location, and recent jobs.

import { api } from "../api.js";
import { lineChart } from "../charts.js";
import { html, setHTML, bytes, num, pct, bar, stateBadge, ago, coalesce } from "../util.js";

export async function render(main, _params, ctx) {
  setHTML(main, html`
    <div class="page-head"><h1>Dashboard</h1></div>
    <div class="grid tiles" id="tiles"></div>
    <div class="grid cols-2 section">
      <div class="card"><h2>CPU usage by location</h2><div id="cpu-chart"></div></div>
      <div class="card"><h2>GPU utilisation by location</h2><div id="gpu-chart"></div></div>
    </div>
    <div class="grid cols-2 section">
      <div class="card"><h2>Pool by location</h2><div id="locations"></div></div>
      <div class="card"><h2>Recent jobs</h2><div id="recent"></div></div>
    </div>`);

  let charts = { locs: "", cpu: null, gpu: null };
  ctx.cleanup(() => { charts.cpu?.destroy(); charts.gpu?.destroy(); });

  const refresh = coalesce(async () => {
    const [status, history, recent] = await Promise.all([
      api.get("/status"), api.get("/pool/history"), api.get("/jobs?limit=6"),
    ]);
    renderTiles(status);
    renderLocations(status);
    renderRecent(recent);
    renderCharts(history, charts);
  });
  await refresh();
  ctx.on("metrics", refresh);
  ctx.on("nodes", refresh);
  ctx.on("jobs", refresh);
}

function tile(label, value, of, percent, sub) {
  return html`<div class="card tile">
    <div class="label">${label}</div>
    <div class="value">${value}${of !== null ? html` <span class="of">/ ${of}</span>` : ""}</div>
    ${percent !== null ? bar(percent, true) : ""}
    <div class="small dim" style="margin-top:6px">${sub}</div>
  </div>`;
}

function renderTiles(s) {
  const t = s.total, u = s.used;
  setHTML(document.getElementById("tiles"), html`
    ${tile("CPU cores in use", num(u.cpus), num(t.cpus), pct(u.cpus, t.cpus), `${num(t.cpus - u.cpus)} free`)}
    ${tile("Memory reserved", bytes(u.memory_bytes), bytes(t.memory_bytes), pct(u.memory_bytes, t.memory_bytes), `${bytes(t.memory_bytes - u.memory_bytes)} free`)}
    ${tile("GPUs in use", u.gpus, t.gpus, t.gpus ? pct(u.gpus, t.gpus) : 0, t.gpus ? `${t.gpus - u.gpus} free` : "no GPUs in the pool")}
    ${tile("Nodes online", s.nodes_online, s.nodes_online + s.nodes_offline, null,
      html`${s.nodes_offline} offline${s.nodes_pending ? html` · <a href="#/nodes">${s.nodes_pending} awaiting approval</a>` : ""}`)}
    ${tile("Tasks running", s.tasks.running + s.tasks.assigned, null, null, `${s.tasks.queued} queued`)}
  `);
}

function renderLocations(s) {
  const el = document.getElementById("locations");
  if (!s.locations?.length) {
    setHTML(el, html`<div class="empty">No nodes online yet. <br><a class="btn" href="#/tokens">Add a node</a></div>`);
    return;
  }
  setHTML(el, html`<div class="table-wrap"><table>
    <thead><tr><th>Location</th><th>CPUs</th><th>Memory</th><th>GPUs</th></tr></thead>
    <tbody>${s.locations.map((loc) => {
      const l = s.by_location[loc];
      return html`<tr><td><b>${loc}</b></td>
        <td>${num(l.used.cpus)} / ${num(l.total.cpus)}${bar(pct(l.used.cpus, l.total.cpus))}</td>
        <td>${bytes(l.used.memory_bytes)} / ${bytes(l.total.memory_bytes)}${bar(pct(l.used.memory_bytes, l.total.memory_bytes))}</td>
        <td>${l.used.gpus} / ${l.total.gpus}</td></tr>`;
    })}</tbody></table></div>`);
}

function renderRecent(jobs) {
  const el = document.getElementById("recent");
  if (!jobs.length) {
    setHTML(el, html`<div class="empty">No jobs yet.<br><a class="btn" href="#/jobs">Submit a job</a></div>`);
    return;
  }
  setHTML(el, html`<div class="table-wrap"><table><tbody>${jobs.map((j) => html`
    <tr class="clickable" data-href="#/jobs/${j.id}">
      <td>${stateBadge(j.state)}</td>
      <td class="truncate"><a href="#/jobs/${j.id}">${j.name}</a></td>
      <td class="nowrap dim">${j.counts.succeeded}/${j.task_count}</td>
      <td class="nowrap faint small">${ago(j.created_at)}</td>
    </tr>`)}</tbody></table></div>`);
}

function renderCharts(history, charts) {
  const locs = [...new Set(history.flatMap((s) => Object.keys(s.loc || {})))].sort();
  const key = locs.join(",");
  if (key !== charts.locs || !charts.cpu) {
    charts.cpu?.destroy();
    charts.gpu?.destroy();
    const series = [{ label: "All nodes" }, ...locs.map((l) => ({ label: l }))];
    const fmt = (v) => v + "%";
    charts.cpu = lineChart(document.getElementById("cpu-chart"), { series, max: 100, fmt });
    charts.gpu = lineChart(document.getElementById("gpu-chart"), { series, max: 100, fmt });
    charts.locs = key;
  }
  const xs = history.map((s) => s.t / 1000);
  charts.cpu.update(xs, history.map((s) => s.cpu), ...locs.map((l) => history.map((s) => s.loc?.[l]?.cpu ?? null)));
  charts.gpu.update(xs, history.map((s) => s.gpu), ...locs.map((l) => history.map((s) => s.loc?.[l]?.gpu ?? null)));
}
