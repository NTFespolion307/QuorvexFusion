// Task view: attempts and the live log of one task.

import { api } from "../api.js";
import { html, setHTML, toast, stateBadge, since, dateTime, coalesce, confirmDialog, bytes, bar, pct } from "../util.js";
import { outputLink } from "./jobs.js";
import { progressText } from "./job.js";

const TERMINAL = new Set(["succeeded", "failed", "canceled"]);

export async function render(main, [id], ctx) {
  const jobId = id.split(".")[0];
  setHTML(main, html`
    <div class="page-head">
      <a href="#/jobs/${jobId}" class="btn ghost sm">← Job ${jobId}</a>
      <h1 class="mono">${id}</h1><span id="state"></span>
      <span class="spacer"></span>
      <button class="btn danger hidden" id="cancel">Cancel task</button>
    </div>
    <div class="card"><div id="info"></div></div>
    <div class="card section">
      <div class="row" style="margin-bottom:10px">
        <div class="tabs" id="streams" style="margin:0;border:none">
          <button data-stream="combined" class="active">Output</button><button data-stream="stdout">stdout</button><button data-stream="stderr">stderr</button>
        </div>
        <span class="spacer" style="flex:1"></span>
        <select id="attempt" style="width:auto"></select>
        <label class="check small"><input type="checkbox" id="follow" checked> Follow</label>
        <button class="btn sm" id="download">Download</button>
      </div>
      <pre class="log" id="log"></pre>
    </div>`);

  let task = null, stream = "combined", attempt = 0, offset = 0, taskState = "", stopped = false;
  const logEl = document.getElementById("log");
  const attemptSel = document.getElementById("attempt");
  const follow = document.getElementById("follow");
  ctx.cleanup(() => { stopped = true; });

  const nodeNames = Object.fromEntries((await api.get("/nodes")).map((n) => [n.id, n.name]));

  const loadTask = async () => {
    task = await api.get("/tasks/" + encodeURIComponent(id));
    setHTML(document.getElementById("state"), stateBadge(task.state));
    document.getElementById("cancel").classList.toggle("hidden", TERMINAL.has(task.state));
    const nodeLink = (nid) => nid ? html`<a href="#/nodes/${nid}">${nodeNames[nid] || nid}</a>` : "-";
    setHTML(document.getElementById("info"), html`
      <dl class="kv">
        <dt>State</dt><dd>${task.state}${task.pending_reason ? html` <span class="dim">(${task.pending_reason})</span>` : ""}</dd>
        <dt>Array index</dt><dd>${task.index}</dd>
        <dt>Node</dt><dd>${nodeLink(task.node_id)}</dd>
        <dt>Exit code</dt><dd>${task.exit_code ?? "-"}</dd>
        ${task.error ? html`<dt>Error</dt><dd>${task.error}</dd>` : ""}
        <dt>Started</dt><dd>${dateTime(task.started_at)}</dd>
        <dt>Duration</dt><dd>${since(task.started_at, task.finished_at)}</dd>
        ${task.progress ? html`<dt>Transfer</dt><dd>${progressText(task.progress)}${bar(pct(task.progress.done_bytes, task.progress.total_bytes), false, true)}</dd>` : ""}
        ${(task.outputs || []).length ? html`<dt>Output files</dt><dd>${task.outputs.map((o) =>
          html`<a href="${outputLink(o)}">${o.path}</a> <span class="faint small">${bytes(o.size)}</span><br>`)}</dd>` : ""}
      </dl>
      ${(task.attempt_list || []).length > 1 ? html`<h3 class="section">Attempts</h3><div class="table-wrap"><table>
        <thead><tr><th>#</th><th>State</th><th>Node</th><th>Exit</th><th>Duration</th><th>Detail</th></tr></thead>
        <tbody>${task.attempt_list.map((a) => html`<tr><td>${a.number}</td><td>${stateBadge(a.state === "lost" ? "offline" : a.state)}</td>
          <td>${nodeLink(a.node_id)}</td><td>${a.exit_code ?? "-"}</td><td>${since(a.started_at, a.finished_at)}</td>
          <td class="truncate">${a.error || ""}</td></tr>`)}</tbody></table></div>` : ""}`);
    // Attempt selector: "latest" follows retries automatically.
    const n = task.attempts;
    const opts = [html`<option value="0">Latest attempt</option>`];
    for (let i = n; i >= 1; i--) opts.push(html`<option value="${i}">Attempt ${i}</option>`);
    const keep = attemptSel.value;
    setHTML(attemptSel, opts);
    attemptSel.value = keep || "0";
    attemptSel.classList.toggle("hidden", n <= 1);
  };

  let shownAttempt = 0;
  let decoder = new TextDecoder();
  const poll = coalesce(async () => {
    const q = new URLSearchParams({ stream, offset: String(offset) });
    if (attempt) q.set("attempt", String(attempt));
    const res = await api.raw(`/tasks/${encodeURIComponent(id)}/logs?${q}`);
    const used = Number(res.headers.get("X-Log-Attempt"));
    const size = Number(res.headers.get("X-Log-Size"));
    taskState = res.headers.get("X-Task-State");
    if (!attempt && used !== shownAttempt) { // a retry started: switch to its log
      logEl.textContent = "";
      offset = 0;
      decoder = new TextDecoder();
      shownAttempt = used;
      return poll();
    }
    // Offsets are in bytes; a streaming decoder keeps multi-byte UTF-8
    // characters split across two responses intact.
    const buf = await res.arrayBuffer();
    if (buf.byteLength) {
      const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
      logEl.append(decoder.decode(buf, { stream: true }));
      offset += buf.byteLength;
      if (follow.checked && atBottom) logEl.scrollTop = logEl.scrollHeight;
    }
    if (offset < size) return poll(); // more is already available
    if (!logEl.textContent && TERMINAL.has(taskState)) logEl.textContent = "(no output)";
    if (!logEl.textContent && !used) logEl.textContent = "(waiting for the task to start)";
  });

  const reset = () => { logEl.textContent = ""; offset = 0; shownAttempt = 0; decoder = new TextDecoder(); poll(); };
  document.getElementById("streams").addEventListener("click", (e) => {
    const b = e.target.closest("[data-stream]");
    if (!b) return;
    document.querySelectorAll("#streams button").forEach((x) => x.classList.toggle("active", x === b));
    stream = b.dataset.stream;
    reset();
  });
  attemptSel.addEventListener("change", () => { attempt = Number(attemptSel.value); reset(); });
  document.getElementById("cancel").addEventListener("click", async () => {
    if (!await confirmDialog("Cancel this task?", "The process is stopped and the task will not be retried.", "Cancel task")) return;
    await api.post(`/tasks/${encodeURIComponent(id)}/cancel`).catch((e) => toast(e.message, "err"));
  });
  document.getElementById("download").addEventListener("click", () => {
    const blob = new Blob([logEl.textContent], { type: "text/plain" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `${id}-${stream}.log`;
    a.click();
    URL.revokeObjectURL(a.href);
  });

  await loadTask();
  await poll();
  // Poll the log while the task can still produce output.
  const timer = setInterval(() => {
    if (stopped) return;
    if (!TERMINAL.has(taskState) || offset === 0) poll();
  }, 1000);
  ctx.cleanup(() => clearInterval(timer));
  ctx.on("jobs", coalesce(loadTask));
  // Transfer progress isn't an event of its own; refresh while it shows.
  const progressTimer = setInterval(() => { if (!stopped && task?.progress) loadTask(); }, 1500);
  ctx.cleanup(() => clearInterval(progressTimer));
}
