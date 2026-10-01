// Nodes: one card per machine with live usage and actions.

import { api } from "../api.js";
import { html, setHTML, bytes, num, pct, meter, stateBadge, ago, duration, toast, modal, confirmDialog, coalesce, kvText, parseKV } from "../util.js";

export async function render(main, _params, ctx) {
  setHTML(main, html`
    <div class="page-head">
      <h1>Nodes</h1>
      <span class="spacer"></span>
      <select id="filter" style="width:auto">
        <option value="">All nodes</option><option value="online">Online</option>
        <option value="offline">Offline</option><option value="pending">Pending approval</option>
        <option value="revoked">Revoked</option>
      </select>
      <input type="text" id="search" placeholder="Search name, location, label" style="width:240px">
      <a class="btn primary" href="#/tokens">Add a node</a>
    </div>
    <div id="cards"></div>`);

  let all = [];
  const cards = document.getElementById("cards");
  const filter = document.getElementById("filter");
  const search = document.getElementById("search");

  const draw = () => {
    const q = search.value.trim().toLowerCase();
    const shown = all.filter((n) => (!filter.value || n.status === filter.value) &&
      (!q || [n.name, n.id, n.location, kvText(n.labels), n.hardware?.cpu_model].join(" ").toLowerCase().includes(q)));
    if (!all.length) {
      setHTML(cards, html`<div class="card empty">No nodes have joined yet.<br>
        <a class="btn primary" href="#/tokens">Create a join code</a></div>`);
      return;
    }
    // Pending first (they need attention), then online, offline, revoked.
    const order = { pending: 0, online: 1, offline: 2, revoked: 3 };
    shown.sort((a, b) => (order[a.status] - order[b.status]) || a.name.localeCompare(b.name));
    setHTML(cards, shown.length ? html`<div class="grid cards">${shown.map(card)}</div>` : html`<div class="card empty">No nodes match.</div>`);
  };

  const refresh = coalesce(async () => { all = await api.get("/nodes"); draw(); });
  filter.addEventListener("change", draw);
  search.addEventListener("input", draw);
  cards.addEventListener("click", (e) => nodeAction(e, all, refresh));
  await refresh();
  ctx.on("nodes", refresh);
  ctx.on("metrics", refresh);
}

export function nodeBadges(n) {
  const hw = n.hardware || {};
  return [
    stateBadge(n.status),
    n.draining ? html`<span class="badge warn">draining</span>` : "",
    n.ephemeral ? html`<span class="badge info">ephemeral</span>` : "",
    hw.in_container ? html`<span class="badge">container</span>` : "",
    n.hardware && !hw.docker ? html`<span class="badge">no docker</span>` : "",
    hw.nvidia_docker ? html`<span class="badge accent">gpu containers</span>` : "",
  ];
}

function card(n) {
  const hw = n.hardware || {}, m = n.metrics || {};
  const online = n.status === "online";
  const memPct = pct(m.mem_used_bytes, m.mem_total_bytes);
  const root = (m.disks || []).find((d) => d.mount === "/") || (m.disks || [])[0];
  const ip = (n.addr || "").replace(/:\d+$/, "");
  return html`<div class="card node-card ${online ? "" : "offline"}">
    <div class="head">
      <a class="name" href="#/nodes/${n.id}" title="${n.id}">${n.name}</a>
      <span class="spacer" style="flex:1"></span>
      ${online ? html`<span class="small dim nowrap">${num(n.rtt_ms)} ms</span>` : html`<span class="small faint nowrap">seen ${ago(n.last_seen)}</span>`}
    </div>
    <div class="badges">${nodeBadges(n)}</div>
    <div class="sub">
      ${n.location ? html`<span>📍 ${n.location}</span>` : ""}
      ${ip ? html`<span class="mono">${ip}</span>` : ""}
      ${m.uptime_seconds ? html`<span>up ${duration(m.uptime_seconds * 1000)}</span>` : ""}
      ${n.version ? html`<span class="faint">version ${n.version}</span>` : ""}
    </div>
    ${hw.cpu_model ? html`<div class="small">${hw.cpu_model} · ${hw.physical_cores}c/${hw.logical_cores}t${
      hw.cpu_limit && hw.cpu_limit < hw.logical_cores ? html` · limit ${num(hw.cpu_limit)}` : ""} · ${bytes(hw.memory_bytes)}</div>` : ""}
    ${online ? html`
      ${meter("CPU", m.cpu_percent || 0, `${num(m.cpu_percent || 0)}%`)}
      ${meter("Memory", memPct, `${bytes(m.mem_used_bytes)}`)}
      ${meter("Reserved", pct(n.used.cpus, hw.cpu_limit), `${num(n.used.cpus)}/${num(hw.cpu_limit)} cpu`)}
      ${root ? meter("Disk " + root.mount, pct(root.used_bytes, root.total_bytes), `${bytes(root.total_bytes - root.used_bytes)} free`) : ""}
      ${(m.gpus || []).map((g) => {
        const info = (hw.gpus || []).find((x) => x.index === g.index) || {};
        return html`<div class="gpu-line"><span>GPU ${g.index} <span class="dim">${info.name || ""}</span></span>
          <span class="dim">${num(g.temperature_c || 0, 0)}°C · ${num(g.power_watts || 0, 0)} W</span></div>
          ${meter("util", g.utilization_percent || 0, `${num(g.utilization_percent || 0, 0)}%`)}
          ${meter("vram", pct(g.memory_used_bytes, g.memory_total_bytes), bytes(g.memory_used_bytes))}`;
      })}
      <div class="small dim">${n.running_tasks} running task${n.running_tasks === 1 ? "" : "s"}</div>` : ""}
    ${Object.keys(n.labels || {}).length ? html`<div class="labels">${Object.entries(n.labels).map(([k, v]) =>
      html`<span class="chip">${k}=${v}</span>`)}</div>` : ""}
    <div class="actions">${actionButtons(n)}</div>
  </div>`;
}

export function actionButtons(n) {
  const b = (action, label, cls = "") => html`<button class="btn sm ${cls}" data-action="${action}" data-id="${n.id}">${label}</button>`;
  return [
    n.status === "pending" ? b("approve", "Approve", "primary") : "",
    n.status === "online" || n.status === "offline" ? b(n.draining ? "undrain" : "drain", n.draining ? "Resume" : "Drain") : "",
    n.status !== "revoked" ? b("labels", "Labels") : "",
    n.status !== "revoked" ? b("revoke", n.status === "pending" ? "Reject" : "Revoke", "danger") : "",
    n.status !== "online" ? b("remove", "Remove", "ghost") : "",
  ];
}

// nodeAction handles the action buttons (also used by the node page).
export async function nodeAction(e, nodes, refresh) {
  const btn = e.target.closest("[data-action]");
  if (!btn) return;
  const n = nodes.find((x) => x.id === btn.dataset.id);
  if (!n) return;
  const path = "/nodes/" + encodeURIComponent(n.id);
  try {
    switch (btn.dataset.action) {
      case "approve":
        await api.post(path + "/approve");
        toast(`${n.name} approved; it connects within a few seconds`, "ok");
        break;
      case "drain":
        if (!await confirmDialog(`Drain ${n.name}?`, "Running tasks finish, but no new tasks are scheduled on this node.", "Drain", false)) return;
        await api.post(path + "/drain", { draining: true });
        break;
      case "undrain":
        await api.post(path + "/drain", { draining: false });
        break;
      case "labels": {
        const worker = Object.entries(n.labels || {}).filter(([k]) => !(k in (n.admin_labels || {})));
        const form = await modal(`Labels of ${n.name}`, html`
          <label class="field">Labels (KEY=VALUE, one per line or comma separated)
            <textarea name="labels" rows="5">${kvText(n.admin_labels, "\n")}</textarea></label>
          <p class="hint">Jobs can require or prefer labels (--require gpu=4090).
          These override labels the worker reports itself${worker.length ? html`: ${worker.map(([k, v]) => html`<span class="chip">${k}=${v}</span> `)}` : ""}.</p>`,
          { okText: "Save" });
        if (!form) return;
        await api.put(path + "/labels", parseKV(form.labels.value));
        toast("Labels saved", "ok");
        break;
      }
      case "revoke":
        if (!await confirmDialog(`Revoke ${n.name}?`, "The node is disconnected immediately and can never reconnect with its current identity. Its running tasks are requeued elsewhere.", "Revoke")) return;
        await api.post(path + "/revoke");
        break;
      case "remove":
        if (!await confirmDialog(`Remove ${n.name}?`, "The node is forgotten. To use the machine again it has to join with a new code.", "Remove")) return;
        await api.del(path);
        if (location.hash.startsWith("#/nodes/")) location.hash = "#/nodes";
        break;
    }
    refresh();
  } catch (err) {
    toast(err.message, "err");
  }
}
