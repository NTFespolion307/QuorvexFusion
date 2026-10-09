// Job detail: spec, progress and the task table.

import { api } from "../api.js";
import { html, setHTML, toast, stateBadge, since, num, bytes, bar, pct, dateTime, coalesce, confirmDialog, kvText } from "../util.js";
import { outputLink } from "./jobs.js";

const PAGE = 500;

export function progressText(p) {
  const what = p.phase === "upload" ? "uploading outputs" : "downloading inputs";
  return `${what} ${pct(p.done_bytes, p.total_bytes).toFixed(0)}% (${bytes(p.done_bytes)} of ${bytes(p.total_bytes)})`;
}

export async function render(main, [id], ctx) {
  let state = "", offset = 0;
  setHTML(main, html`
    <div class="page-head">
      <a href="#/jobs" class="btn ghost sm">← Jobs</a>
      <h1 class="mono">${id}</h1><span id="state"></span>
      <span class="spacer"></span>
      <button class="btn danger hidden" id="cancel">Cancel job</button>
      <button class="btn ghost hidden" id="delete">Delete</button>
    </div>
    <div class="grid cols-2">
      <div class="card"><h2 id="name"></h2><div id="spec"></div></div>
      <div class="card"><h2>Progress</h2><div id="progress"></div></div>
    </div>
    <div class="card section hidden" id="outputs-card">
      <div class="row" style="margin-bottom:10px"><h2 style="margin:0">Output files</h2><span style="flex:1"></span>
        <a class="btn sm" id="zip">Download all (zip)</a></div>
      <div id="outputs"></div>
    </div>
    <div class="card section">
      <div class="row" style="margin-bottom:10px"><h2 style="margin:0">Tasks</h2><span class="spacer" style="flex:1"></span>
        <select id="state-filter" style="width:auto">
          <option value="">All states</option><option>queued</option><option>running</option>
          <option>succeeded</option><option>failed</option><option>canceled</option>
        </select></div>
      <div id="tasks"></div>
      <div class="row hidden" id="pager" style="margin-top:10px">
        <button class="btn sm" id="prev">Previous</button><span class="small dim" id="page-info"></span><button class="btn sm" id="next">Next</button>
      </div>
    </div>`);

  let nodeNames = {};
  const loadNodes = async () => {
    nodeNames = Object.fromEntries((await api.get("/nodes")).map((n) => [n.id, n.name]));
  };

  const refresh = coalesce(async () => {
    const [job, tasks] = await Promise.all([
      api.get("/jobs/" + encodeURIComponent(id)),
      api.get(`/jobs/${encodeURIComponent(id)}/tasks?limit=${PAGE}&offset=${offset}${state ? "&state=" + state : ""}`),
    ]);
    if (tasks.some((t) => t.node_id && !nodeNames[t.node_id])) await loadNodes();
    drawJob(job);
    drawTasks(tasks, job);
    if (job.spec.outputs?.length) drawOutputs(await api.get(`/jobs/${encodeURIComponent(id)}/outputs`), job);
  });

  const drawOutputs = (outs, j) => {
    document.getElementById("outputs-card").classList.remove("hidden");
    document.getElementById("zip").href = `/api/v1/jobs/${encodeURIComponent(id)}/outputs.zip`;
    const totalBytes = outs.reduce((n, o) => n + o.size, 0);
    const shown = outs.slice(0, 200);
    setHTML(document.getElementById("outputs"), outs.length ? html`
      <p class="small dim">${outs.length} file(s), ${bytes(totalBytes)}${outs.length > shown.length ? " (first 200 listed)" : ""}.
        From a terminal: <code>cluster outputs ${id}</code></p>
      <div class="table-wrap"><table><thead><tr>${j.task_count > 1 ? html`<th>Task</th>` : ""}<th>File</th><th>Size</th></tr></thead>
      <tbody>${shown.map((o) => html`<tr>${j.task_count > 1 ? html`<td class="mono">${o.index}</td>` : ""}
        <td class="truncate"><a href="${outputLink(o)}">${o.path}</a></td><td class="nowrap">${bytes(o.size)}</td></tr>`)}</tbody></table></div>`
      : html`<div class="empty">No output files yet (patterns: ${j.spec.outputs.join(", ")}).</div>`);
  };

  const drawJob = (j) => {
    const s = j.spec, c = j.counts;
    const active = c.queued + c.running + c.assigned > 0;
    setHTML(document.getElementById("state"), stateBadge(j.state));
    document.getElementById("cancel").classList.toggle("hidden", !active);
    document.getElementById("delete").classList.toggle("hidden", active);
    document.getElementById("name").textContent = j.name;
    setHTML(document.getElementById("spec"), html`<dl class="kv">
      ${s.image ? html`<dt>Image</dt><dd class="mono">${s.image}</dd>` : ""}
      <dt>Command</dt><dd>${s.command ? html`<code>${s.command}</code>` : html`<span class="dim">the image's default</span>`}</dd>
      <dt>Per task</dt><dd>${num(s.cpus)} CPU · ${s.memory_bytes ? bytes(s.memory_bytes) : "memory not reserved"} · ${s.gpus || 0} GPU</dd>
      ${s.array ? html`<dt>Array</dt><dd>${s.array}</dd>` : ""}
      <dt>Retries</dt><dd>${s.retries || 0}</dd>
      <dt>Timeout</dt><dd>${s.timeout_seconds ? s.timeout_seconds + " s" : "none"}</dd>
      ${s.priority ? html`<dt>Priority</dt><dd>${s.priority}</dd>` : ""}
      ${s.env ? html`<dt>Environment</dt><dd class="mono small">${kvText(s.env, "  ")}</dd>` : ""}
      ${s.inputs?.length ? html`<dt>Inputs</dt><dd>${s.inputs.length} file(s)${s.inputs.some((i) => i.size) ? ", " + bytes(s.inputs.reduce((n, i) => n + (i.size || 0), 0)) : ""}:
        <span class="small dim">${s.inputs.slice(0, 8).map((i) => i.path).join(", ")}${s.inputs.length > 8 ? ", …" : ""}</span></dd>` : ""}
      ${s.outputs?.length ? html`<dt>Outputs</dt><dd class="mono small">${s.outputs.join("  ")}</dd>` : ""}
      ${s.requires ? html`<dt>Requires</dt><dd>${kvText(s.requires)}</dd>` : ""}
      ${s.prefers ? html`<dt>Prefers</dt><dd>${kvText(s.prefers)}</dd>` : ""}
      ${s.allow_ephemeral === false ? html`<dt>Ephemeral nodes</dt><dd>not allowed</dd>` : ""}
      ${s.allow_remote === false ? html`<dt>Remote nodes</dt><dd>not allowed</dd>` : ""}
      <dt>Submitted</dt><dd>${dateTime(j.created_at)}</dd>
    </dl>`);
    setHTML(document.getElementById("progress"), html`
      <div class="tile"><div class="value">${c.succeeded} <span class="of">/ ${j.task_count} succeeded</span></div></div>
      ${bar(pct(c.succeeded, j.task_count), true, true)}
      <dl class="kv" style="margin-top:14px">
        <dt>Running</dt><dd>${c.running + c.assigned}</dd><dt>Queued</dt><dd>${c.queued}</dd>
        <dt>Failed</dt><dd>${c.failed}</dd><dt>Canceled</dt><dd>${c.canceled}</dd>
      </dl>`);
  };

  const drawTasks = (tasks, j) => {
    const total = state ? null : j.task_count;
    setHTML(document.getElementById("tasks"), tasks.length ? html`<div class="table-wrap"><table>
      <thead><tr><th>Task</th><th>State</th><th>Attempts</th><th>Node</th><th>Exit</th><th>Duration</th><th>Detail</th></tr></thead>
      <tbody>${tasks.map((t) => html`<tr class="clickable" data-href="#/tasks/${t.id}">
        <td class="mono"><a href="#/tasks/${t.id}">${t.id}</a></td>
        <td>${stateBadge(t.state)}</td><td>${t.attempts}</td>
        <td class="nowrap">${t.node_id ? html`<a href="#/nodes/${t.node_id}">${nodeNames[t.node_id] || t.node_id}</a>` : "-"}</td>
        <td>${t.exit_code ?? "-"}</td>
        <td class="nowrap">${since(t.started_at, t.finished_at)}</td>
        <td class="truncate ${t.error ? "" : "dim"}">${t.progress ? progressText(t.progress) : (t.pending_reason || t.error || "")}</td>
      </tr>`)}</tbody></table></div>` : html`<div class="empty">No tasks${state ? " in this state" : ""}.</div>`);
    const pager = document.getElementById("pager");
    const more = tasks.length === PAGE;
    pager.classList.toggle("hidden", offset === 0 && !more);
    document.getElementById("prev").disabled = offset === 0;
    document.getElementById("next").disabled = !more;
    document.getElementById("page-info").textContent = `tasks ${offset + 1}–${offset + tasks.length}${total ? " of " + total : ""}`;
  };

  document.getElementById("state-filter").addEventListener("change", (e) => { state = e.target.value; offset = 0; refresh(); });
  document.getElementById("prev").addEventListener("click", () => { offset = Math.max(0, offset - PAGE); refresh(); });
  document.getElementById("next").addEventListener("click", () => { offset += PAGE; refresh(); });
  document.getElementById("cancel").addEventListener("click", async () => {
    if (!await confirmDialog("Cancel this job?", "Queued tasks are dropped and running tasks are stopped.", "Cancel job")) return;
    await api.post(`/jobs/${encodeURIComponent(id)}/cancel`).catch((e) => toast(e.message, "err"));
  });
  document.getElementById("delete").addEventListener("click", async () => {
    if (!await confirmDialog("Delete this job?", "The job, its tasks and their logs are removed permanently.", "Delete")) return;
    try {
      await api.del("/jobs/" + encodeURIComponent(id));
      location.hash = "#/jobs";
    } catch (e) {
      toast(e.message, "err");
    }
  });

  await loadNodes();
  await refresh();
  ctx.on("jobs", refresh);
}
