// Node detail: full hardware, live per-core CPU, and history charts for
// CPU, memory, network and every GPU.

import { api } from "../api.js";
import { lineChart } from "../charts.js";
import { html, setHTML, bytes, num, pct, bar, ago, duration, dateTime, coalesce } from "../util.js";
import { nodeBadges, actionButtons, nodeAction } from "./nodes.js";

export async function render(main, [id], ctx) {
  let n = await api.get("/nodes/" + encodeURIComponent(id));
  let history = await api.get("/nodes/" + encodeURIComponent(id) + "/history");

  const gpus = n.hardware?.gpus || [];
  setHTML(main, html`
    <div class="page-head">
      <a href="#/nodes" class="btn ghost sm">← Nodes</a>
      <h1>${n.name}</h1><span id="badges" class="row"></span>
      <span class="spacer"></span><span id="actions" class="row"></span>
    </div>
    <div class="grid cols-2">
      <div class="card"><h2>Machine</h2><div id="hw"></div></div>
      <div class="card"><h2>CPU cores <span class="dim small" id="cpu-now"></span></h2><div id="cores" class="cores"></div>
        <div class="section"><h3>Disks</h3><div id="disks"></div></div></div>
    </div>
    <div class="grid cols-2 section">
      <div class="card"><h2>CPU</h2><div id="c-cpu"></div></div>
      <div class="card"><h2>Memory</h2><div id="c-mem"></div></div>
      <div class="card"><h2>Network</h2><div id="c-net"></div></div>
      ${gpus.map((g) => html`<div class="card"><h2>GPU ${g.index} · ${g.name}</h2><div id="c-gpu-${g.index}"></div></div>`)}
    </div>`);

  const pctFmt = (v) => v + "%";
  const charts = {
    cpu: lineChart(document.getElementById("c-cpu"), { series: [{ label: "CPU %" }, { label: "Load (1 min) as % of cores", color: "--chart-3" }], max: 100, fmt: pctFmt }),
    mem: lineChart(document.getElementById("c-mem"), { series: [{ label: "Used", color: "--chart-2" }], fmt: (v) => bytes(v), axisWidth: 76, minTop: 64 * 1024 * 1024 }),
    net: lineChart(document.getElementById("c-net"), { series: [{ label: "Receive" }, { label: "Send", color: "--chart-4" }], fmt: (v) => bytes(v) + "/s", axisWidth: 84, minTop: 10 * 1024 }),
    gpu: gpus.map((g) => lineChart(document.getElementById("c-gpu-" + g.index), {
      series: [{ label: "Utilisation %" }, { label: "Memory %", color: "--chart-2" }, { label: "Temperature °C", color: "--chart-5" }],
      max: 100, fmt: (v) => String(v),
    })),
  };
  ctx.cleanup(() => { charts.cpu.destroy(); charts.mem.destroy(); charts.net.destroy(); charts.gpu.forEach((c) => c.destroy()); });

  const drawCharts = () => {
    const xs = history.map((m) => m.time_unix_ms / 1000);
    const cores = n.hardware?.logical_cores || 1;
    charts.cpu.update(xs, history.map((m) => m.cpu_percent || 0), history.map((m) => Math.min(100, (100 * (m.load1 || 0)) / cores)));
    charts.mem.update(xs, history.map((m) => m.mem_used_bytes || 0));
    charts.net.update(xs, history.map((m) => m.net_rx_bytes_per_sec || 0), history.map((m) => m.net_tx_bytes_per_sec || 0));
    gpus.forEach((g, i) => {
      const get = (m) => (m.gpus || []).find((x) => x.index === g.index) || {};
      charts.gpu[i].update(xs,
        history.map((m) => get(m).utilization_percent || 0),
        history.map((m) => pct(get(m).memory_used_bytes, get(m).memory_total_bytes)),
        history.map((m) => get(m).temperature_c ?? null));
    });
  };

  const drawInfo = () => {
    const hw = n.hardware || {}, m = n.metrics || {};
    setHTML(document.getElementById("badges"), nodeBadges(n));
    setHTML(document.getElementById("actions"), actionButtons(n));
    setHTML(document.getElementById("hw"), html`<dl class="kv">
      <dt>Node ID</dt><dd class="mono">${n.id}</dd>
      <dt>Status</dt><dd>${n.status}${n.status === "online" ? html` · ${num(n.rtt_ms)} ms round trip · connected ${ago(n.connected_at)}` : html` · last seen ${ago(n.last_seen)}`}</dd>
      <dt>Address</dt><dd class="mono">${n.addr || "-"}</dd>
      <dt>Location</dt><dd>${n.location || "-"}</dd>
      <dt>OS</dt><dd>${hw.os || "-"} · kernel ${hw.kernel || "-"} · ${hw.arch || ""}</dd>
      <dt>CPU</dt><dd>${hw.cpu_model || "-"}<br><span class="dim">${hw.physical_cores} cores / ${hw.logical_cores} threads${hw.cpu_limit < hw.logical_cores ? html`, limited to ${num(hw.cpu_limit)} by cgroup` : ""}</span></dd>
      <dt>Memory</dt><dd>${bytes(hw.memory_bytes)}</dd>
      <dt>GPUs</dt><dd>${(hw.gpus || []).length ? (hw.gpus || []).map((g) => html`${g.index}: ${g.name} (${bytes(g.memory_bytes)}${g.driver ? ", driver " + g.driver : ""})<br>`) : "none"}</dd>
      <dt>Containers</dt><dd>Docker ${hw.docker ? "available" : "not available"}${hw.nvidia_docker ? " · NVIDIA toolkit" : ""}${hw.in_container ? " · worker runs inside a container" : ""}</dd>
      <dt>Limits</dt><dd>${hw.systemd ? "systemd (CPU/memory limits enforced)" : "no systemd (limits not enforced)"}</dd>
      <dt>IPs</dt><dd class="mono">${(hw.ips || []).join("  ") || "-"}</dd>
      <dt>Running tasks</dt><dd>${n.running_tasks} · reserved ${num(n.used.cpus)} CPU, ${bytes(n.used.memory_bytes)}, ${n.used.gpus} GPU</dd>
      <dt>Uptime</dt><dd>${m.uptime_seconds ? duration(m.uptime_seconds * 1000) : "-"}${hw.boot_time_unix ? html` <span class="faint">(booted ${dateTime(hw.boot_time_unix * 1000)})</span>` : ""}</dd>
      <dt>Labels</dt><dd>${Object.entries(n.labels || {}).map(([k, v]) => html`<span class="chip">${k}=${v}</span> `)}</dd>
      <dt>Version</dt><dd>${n.version || "-"}</dd>
      <dt>Joined</dt><dd>${dateTime(n.created_at)}</dd>
    </dl>`);

    const per = m.cpu_per_core || [];
    document.getElementById("cpu-now").textContent = n.status === "online" ? `${num(m.cpu_percent || 0)}% overall` : "";
    setHTML(document.getElementById("cores"), per.length ? per.map((p, i) => html`<div class="core" title="core ${i}: ${num(p)}%">
      <span style="height:${Math.min(100, p).toFixed(0)}%"></span><em>${num(p, 0)}</em></div>`) : html`<span class="dim small">No live data.</span>`);
    setHTML(document.getElementById("disks"), (m.disks || []).map((d) => html`
      <div class="row" style="justify-content:space-between"><span class="mono small">${d.mount}</span>
      <span class="small dim">${bytes(d.used_bytes)} / ${bytes(d.total_bytes)}</span></div>${bar(pct(d.used_bytes, d.total_bytes))}`));
  };

  drawInfo();
  drawCharts();

  // Live updates: fetch the node (small) and append its newest sample.
  const refresh = coalesce(async () => {
    n = await api.get("/nodes/" + encodeURIComponent(id));
    const m = n.metrics;
    if (m && (!history.length || m.time_unix_ms > history[history.length - 1].time_unix_ms)) {
      history.push(m);
      if (history.length > 720) history.shift();
      drawCharts();
    }
    drawInfo();
  });
  ctx.on("metrics", refresh);
  ctx.on("nodes", refresh);
  // <main> outlives this page, so the listener must be removed on navigation.
  const onClick = (e) => nodeAction(e, [n], refresh);
  main.addEventListener("click", onClick);
  ctx.cleanup(() => main.removeEventListener("click", onClick));
}
