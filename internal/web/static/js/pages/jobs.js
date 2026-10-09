// Jobs: submit form and the list of recent jobs.

import { api } from "../api.js";
import { html, setHTML, toast, stateBadge, ago, num, bytes, bar, pct, parseKV, coalesce, confirmDialog } from "../util.js";
export { outputLink };

// Link to download one output file of a task.
function outputLink(o) {
  const path = o.path.split("/").map(encodeURIComponent).join("/");
  return `/api/v1/tasks/${encodeURIComponent(o.task_id)}/files/${path}`;
}

export async function render(main, _params, ctx) {
  setHTML(main, html`
    <div class="page-head">
      <h1>Jobs</h1><span class="spacer"></span>
      <button class="btn primary" id="toggle-form">New job</button>
    </div>
    <form class="card stack hidden" id="submit">
      <h2>Submit a job</h2>
      <label class="field"><span id="command-label">Command (runs with /bin/sh -c; {i} is replaced by the array index)</span>
        <textarea name="command" rows="3" placeholder="python3 simulate.py --seed {i}"></textarea></label>
      <div class="form-grid">
        <label class="field">Name <input type="text" name="name" placeholder="defaults to the command"></label>
        <label class="field">Docker image <input type="text" name="image" placeholder="optional, e.g. python:3.12-slim"></label>
        <label class="field">CPUs per task <input type="number" name="cpus" value="1" min="0.1" step="0.1"></label>
        <label class="field">Memory per task <input type="text" name="memory" placeholder="e.g. 4G (optional)"></label>
        <label class="field">GPUs per task <input type="number" name="gpus" value="0" min="0" step="1"></label>
        <label class="field">Copies / array <input type="text" name="array" placeholder="16, or a range 1-500">
          <span class="hint" id="array-hint">1 task</span></label>
        <label class="field">Retries <input type="number" name="retries" value="0" min="0" max="100"></label>
        <label class="field">Timeout <input type="text" name="timeout" placeholder="e.g. 30m, 2h"></label>
        <label class="field">Priority <input type="number" name="priority" value="0"></label>
        <label class="field wide">Environment (KEY=VALUE per line)
          <textarea name="env" rows="2" placeholder="MODEL=small"></textarea></label>
        <label class="field">Require node labels <input type="text" name="require" placeholder="gpu=4090, location=home"></label>
        <label class="field">Prefer node labels <input type="text" name="prefer" placeholder="location=home"></label>
        <div class="stack wide" style="gap:6px">
          <label class="check"><input type="checkbox" name="noephemeral"> Never run on ephemeral (rented/cloud) nodes</label>
          <label class="check"><input type="checkbox" name="noremote"> Never run on remote nodes</label>
        </div>
      </div>
      <h3 class="section">Files</h3>
      <div class="form-grid">
        <label class="field">Script to run (optional)
          <input type="file" name="script">
          <span class="hint">Uploaded and run; the command box then holds its arguments.</span></label>
        <label class="field">Input files <input type="file" name="files" multiple>
          <span class="hint">Placed in each task's working directory.</span></label>
        <label class="field">Input folder <input type="file" name="folder" webkitdirectory multiple>
          <span class="hint">Kept as a folder of the same name.</span></label>
        <label class="field">Outputs to collect
          <input type="text" name="outputs" placeholder="frames/*.png, results/**">
          <span class="hint">Globs relative to the working directory.</span></label>
      </div>
      <div class="hidden" id="upload-progress"><div class="small dim" id="upload-text"></div><div class="bar thick"><span id="upload-bar" class="ok" style="width:0%"></span></div></div>
      <p class="hint">With a Docker image, the command runs inside the container in <code>/work</code>, where the input files are;
        GPU containers need the NVIDIA container toolkit on the node.</p>
      <div class="row"><button class="btn primary" type="submit">Submit</button>
        <button class="btn ghost" type="button" id="cancel-form">Cancel</button></div>
    </form>
    <div class="card" id="list"></div>`);

  const form = document.getElementById("submit");
  const toggle = (show) => form.classList.toggle("hidden", !show);
  document.getElementById("toggle-form").addEventListener("click", () => { toggle(form.classList.contains("hidden")); form.command.focus(); });
  document.getElementById("cancel-form").addEventListener("click", () => toggle(false));
  // Show how many tasks the array field produces while typing.
  form.array.addEventListener("input", () => {
    const n = countArray(form.array.value);
    document.getElementById("array-hint").textContent = n === null ? "e.g. 16, 1-500, 0-99:10 or 1,5,9" : `${n} task${n === 1 ? "" : "s"}`;
  });
  form.script.addEventListener("change", () => {
    document.getElementById("command-label").textContent = form.script.files.length
      ? `Arguments for ${form.script.files[0].name} ({i} is replaced by the array index)`
      : "Command (runs with /bin/sh -c; {i} is replaced by the array index)";
  });
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    try {
      const spec = specFromForm(form.elements);
      const script = form.script.files[0];
      if (!script && !spec.command && !spec.image) throw new Error("Enter a command, choose a script, or give a Docker image");
      spec.inputs = await uploadInputs(form);
      if (script) spec.command = (await scriptCommand(script)) + (spec.command ? " " + spec.command : "");
      const job = await api.post("/jobs", spec);
      toast(`Submitted ${job.id} (${job.task_count} task${job.task_count === 1 ? "" : "s"})`, "ok");
      location.hash = "#/jobs/" + job.id;
    } catch (err) {
      toast(err.message, "err");
    } finally {
      submit.disabled = false;
    }
  });

  const list = document.getElementById("list");
  list.addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-cancel]");
    if (!btn) return;
    if (!await confirmDialog("Cancel this job?", "Queued tasks are dropped and running tasks are stopped.", "Cancel job")) return;
    await api.post(`/jobs/${encodeURIComponent(btn.dataset.cancel)}/cancel`).catch((err) => toast(err.message, "err"));
  });

  const refresh = coalesce(async () => {
    const jobs = await api.get("/jobs?limit=200");
    if (!jobs.length) {
      setHTML(list, html`<div class="empty">No jobs yet. Submit one with <b>New job</b>, or from a terminal:<br>
        <code>cluster submit -f -- echo hello</code></div>`);
      return;
    }
    setHTML(list, html`<div class="table-wrap"><table>
      <thead><tr><th>Job</th><th>State</th><th>Progress</th><th>Running</th><th>Queued</th><th>Failed</th><th>Per task</th><th>Submitted</th><th>Name</th><th></th></tr></thead>
      <tbody>${jobs.map((j) => {
        const c = j.counts;
        const active = c.queued + c.running + c.assigned > 0;
        return html`<tr class="clickable" data-href="#/jobs/${j.id}">
          <td class="mono"><a href="#/jobs/${j.id}">${j.id}</a></td>
          <td>${stateBadge(j.state)}</td>
          <td style="min-width:120px"><span class="small dim">${c.succeeded}/${j.task_count}</span>${bar(pct(c.succeeded, j.task_count), false, true)}</td>
          <td>${c.running + c.assigned}</td><td>${c.queued}</td>
          <td>${c.failed ? html`<span class="badge err">${c.failed}</span>` : "0"}</td>
          <td class="nowrap small">${num(j.spec.cpus)} CPU${j.spec.memory_bytes ? ", " + bytes(j.spec.memory_bytes) : ""}${j.spec.gpus ? `, ${j.spec.gpus} GPU` : ""}</td>
          <td class="nowrap faint small">${ago(j.created_at)}</td>
          <td class="truncate">${j.name}</td>
          <td class="right">${active ? html`<button class="btn sm danger" data-cancel="${j.id}">Cancel</button>` : ""}</td>
        </tr>`;
      })}</tbody></table></div>`);
  });
  await refresh();
  ctx.on("jobs", refresh);
}

// --- input files ---------------------------------------------------------

// uploadInputs sends the chosen files to the controller (which hashes and
// de-duplicates them) and returns the job's input list.
async function uploadInputs(form) {
  const items = [];
  const script = form.script.files[0];
  if (script) items.push({ file: script, path: script.name, mode: 0o755 });
  for (const f of form.files.files) items.push({ file: f, path: f.name });
  for (const f of form.folder.files) items.push({ file: f, path: f.webkitRelativePath || f.name });
  if (!items.length) return [];

  const total = items.reduce((n, it) => n + it.file.size, 0);
  const box = document.getElementById("upload-progress");
  const text = document.getElementById("upload-text");
  const barEl = document.getElementById("upload-bar");
  box.classList.remove("hidden");
  let done = 0;
  const inputs = [];
  try {
    for (const it of items) {
      text.textContent = `Uploading ${it.path}`;
      const res = await uploadOne(it.file, (n) => {
        barEl.style.width = (100 * (done + n) / Math.max(total, 1)).toFixed(1) + "%";
      });
      done += it.file.size;
      inputs.push({ path: it.path, sha256: res.sha256, size: res.size, mode: it.mode || 0o644 });
    }
    text.textContent = `Uploaded ${items.length} file(s), ${bytes(total)}`;
  } catch (err) {
    box.classList.add("hidden");
    throw err;
  }
  return inputs;
}

// uploadOne posts a file with upload progress (fetch can't report it).
function uploadOne(file, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/v1/blobs");
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (xhr.status === 401) { location.href = "/login"; return; }
      let body = {};
      try { body = JSON.parse(xhr.responseText); } catch (e) { /* not JSON */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(body);
      else reject(new Error(body.error || `upload of ${file.name} failed (${xhr.status})`));
    };
    xhr.onerror = () => reject(new Error(`upload of ${file.name} failed: network error`));
    xhr.send(file);
  });
}

function shellQuote(s) {
  return /^[A-Za-z0-9_@%+=:,./-]+$/.test(s) ? s : "'" + s.replace(/'/g, "'\"'\"'") + "'";
}

// scriptCommand mirrors the CLI: run directly with a "#!" line, else pick
// an interpreter from the extension.
async function scriptCommand(file) {
  const name = shellQuote(file.name);
  if ((await file.slice(0, 2).text()) === "#!") return "./" + name;
  const ext = file.name.split(".").pop().toLowerCase();
  const interp = { py: "python3", sh: "bash", bash: "bash", r: "Rscript", js: "node", pl: "perl" }[ext];
  return interp ? `${interp} ${name}` : "./" + name;
}

// --- form to JobSpec -----------------------------------------------------

// countArray mirrors the server's ParseArray just enough to preview the
// task count (null when the text is not valid yet).
export function countArray(s) {
  s = String(s || "").trim();
  if (!s) return 1;
  if (/^\d+$/.test(s)) return Number(s) >= 1 ? Number(s) : null;
  const seen = new Set();
  for (const part of s.split(",").map((p) => p.trim()).filter(Boolean)) {
    const m = part.match(/^(\d+)(?:-(\d+))?(?::(\d+))?$/);
    if (!m) return null;
    const lo = Number(m[1]), hi = m[2] === undefined ? lo : Number(m[2]), step = Number(m[3] || 1);
    if (hi < lo || step < 1 || hi - lo > 200000) return null;
    for (let i = lo; i <= hi; i += step) seen.add(i);
  }
  return seen.size || null;
}

export function parseSize(s) {
  s = String(s || "").trim().toUpperCase().replace(/I?B$/, "");
  if (!s) return 0;
  const m = s.match(/^([\d.]+)\s*([KMGT]?)$/);
  if (!m) throw new Error(`memory "${s}": use e.g. 512M or 4G`);
  return Math.round(Number(m[1]) * { "": 1, K: 1024, M: 1024 ** 2, G: 1024 ** 3, T: 1024 ** 4 }[m[2]]);
}

export function parseDurationSec(s) {
  s = String(s || "").trim();
  if (!s) return 0;
  let total = 0, matched = "";
  for (const m of s.matchAll(/(\d+(?:\.\d+)?)\s*(h|m|s)/g)) {
    total += Number(m[1]) * { h: 3600, m: 60, s: 1 }[m[2]];
    matched += m[0];
  }
  if (!total || matched.replace(/\s/g, "") !== s.replace(/\s/g, "")) throw new Error(`timeout "${s}": use e.g. 90s, 30m or 2h`);
  return Math.round(total);
}

function specFromForm(f) {
  const spec = {
    command: f.command.value.trim(),
    name: f.name.value.trim(),
    image: f.image.value.trim(),
    cpus: Number(f.cpus.value) || 1,
    memory_bytes: parseSize(f.memory.value),
    gpus: Number(f.gpus.value) || 0,
    array: f.array.value.trim(),
    retries: Number(f.retries.value) || 0,
    timeout_seconds: parseDurationSec(f.timeout.value),
    priority: Number(f.priority.value) || 0,
    env: parseKV(f.env.value),
    requires: parseKV(f.require.value),
    prefers: parseKV(f.prefer.value),
    outputs: f.outputs.value.split(/[,\n]/).map((s) => s.trim()).filter(Boolean),
  };
  if (f.noephemeral.checked) spec.allow_ephemeral = false;
  if (f.noremote.checked) spec.allow_remote = false;
  return spec;
}
