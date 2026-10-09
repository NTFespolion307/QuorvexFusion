// Files: the controller's file library. Upload files and folders from
// wherever the browser is, browse them, and use them as job inputs.

import { api } from "../api.js";
import { uploadFile } from "../upload.js";
import { html, setHTML, toast, bytes, dateTime, bar, pct, coalesce, confirmDialog } from "../util.js";

// Interpreters for scripts in the library. Library files are placed
// read-only (not executable), so scripts are started through these.
export const INTERPRETERS = { py: "python3", sh: "bash", bash: "bash", r: "Rscript", js: "node", pl: "perl", rb: "ruby", jl: "julia" };

export function runCommand(path) {
  const ext = path.split(".").pop().toLowerCase();
  const quoted = /^[A-Za-z0-9_@%+=:,./-]+$/.test(path) ? path : "'" + path.replace(/'/g, `'"'"'`) + "'";
  return `${INTERPRETERS[ext] || "bash"} ${quoted}`;
}

const isScript = (path) => path.split(".").pop().toLowerCase() in INTERPRETERS;

// Library paths are "a/b/c.txt"; folders are path prefixes.
function encodePath(p) { return encodeURIComponent(p); }

export async function render(main, _params, ctx) {
  let folder = sessionStorage.getItem("files-folder") || "";
  let all = [];

  setHTML(main, html`
    <div class="page-head"><h1>Files</h1><span class="spacer"></span>
      <span class="small dim" id="totals"></span></div>
    <div class="card stack" id="upload-card">
      <h2>Upload</h2>
      <div class="form-grid">
        <label class="field">Into folder <input type="text" id="dest" placeholder="(top level)"></label>
        <label class="field">Files <input type="file" id="pick-files" multiple></label>
        <label class="field">A whole folder <input type="file" id="pick-folder" webkitdirectory multiple></label>
      </div>
      <p class="hint">Uploads continue after connection drops; if one stops, pick the same file again to resume.
        Files here can be used by any number of jobs (New job → "Inputs from your files") without uploading again.</p>
      <div id="queue"></div>
    </div>
    <div class="card section">
      <div class="row" style="margin-bottom:10px"><h2 style="margin:0" id="crumbs"></h2></div>
      <div id="list"></div>
    </div>`);

  const dest = document.getElementById("dest");
  dest.value = folder;
  const queue = document.getElementById("queue");

  const draw = () => {
    sessionStorage.setItem("files-folder", folder);
    const total = all.reduce((n, f) => n + f.size, 0);
    document.getElementById("totals").textContent = `${all.length} file(s), ${bytes(total)}`;
    // Breadcrumbs.
    const parts = folder ? folder.split("/") : [];
    setHTML(document.getElementById("crumbs"), html`<a href="#" data-folder="">Files</a>${parts.map((p, i) =>
      html` / <a href="#" data-folder="${parts.slice(0, i + 1).join("/")}">${p}</a>`)}`);
    // Direct subfolders and files of the current folder.
    const prefix = folder ? folder + "/" : "";
    const sub = new Map();
    const here = [];
    for (const f of all) {
      if (!f.path.startsWith(prefix)) continue;
      const rest = f.path.slice(prefix.length);
      const slash = rest.indexOf("/");
      if (slash < 0) { here.push(f); continue; }
      const name = rest.slice(0, slash);
      const s = sub.get(name) || { count: 0, size: 0 };
      s.count++; s.size += f.size;
      sub.set(name, s);
    }
    const list = document.getElementById("list");
    if (!sub.size && !here.length) {
      setHTML(list, html`<div class="empty">${folder ? "This folder is empty." : "No files yet. Upload some above."}</div>`);
      return;
    }
    setHTML(list, html`<div class="table-wrap"><table>
      <thead><tr><th>Name</th><th>Size</th><th>Uploaded</th><th></th></tr></thead>
      <tbody>
        ${[...sub.entries()].sort().map(([name, s]) => html`<tr>
          <td><a href="#" data-folder="${prefix + name}">📁 ${name}/</a></td>
          <td class="nowrap">${bytes(s.size)} <span class="faint small">(${s.count} files)</span></td><td></td>
          <td class="right"><button class="btn sm danger" data-delete="${prefix + name}" data-kind="folder">Delete</button></td></tr>`)}
        ${here.map((f) => html`<tr>
          <td class="truncate"><a href="/api/v1/files/content?path=${encodePath(f.path)}" title="download">${f.path.slice(prefix.length)}</a></td>
          <td class="nowrap">${bytes(f.size)}</td><td class="nowrap faint small">${dateTime(f.uploaded_at)}</td>
          <td class="right nowrap">${isScript(f.path) ? html`<button class="btn sm primary" data-run="${f.path}">Run</button> ` : ""}<button class="btn sm danger" data-delete="${f.path}" data-kind="file">Delete</button></td></tr>`)}
      </tbody></table></div>`);
  };

  const refresh = coalesce(async () => { all = await api.get("/files"); draw(); });

  // <main> outlives this page: the listener is removed on navigation.
  const onClick = async (e) => {
    const nav = e.target.closest("[data-folder]");
    if (nav) {
      e.preventDefault();
      folder = nav.dataset.folder;
      dest.value = folder;
      draw();
      return;
    }
    const run = e.target.closest("[data-run]");
    if (run) {
      // The jobs page picks this up and opens a prefilled New job form.
      sessionStorage.setItem("run-library-file", run.dataset.run);
      location.hash = "#/jobs";
      return;
    }
    const del = e.target.closest("[data-delete]");
    if (!del) return;
    const p = del.dataset.delete;
    const what = del.dataset.kind === "folder" ? `the folder ${p}/ and everything in it` : p;
    if (!await confirmDialog("Delete from the library?", `This deletes ${what}. Jobs that already used the files keep them.`, "Delete")) return;
    try {
      await api.del("/files?path=" + encodePath(p));
      refresh();
    } catch (err) {
      toast(err.message, "err");
    }
  };
  main.addEventListener("click", onClick);
  ctx.cleanup(() => main.removeEventListener("click", onClick));

  // Uploading: one row per file with its own progress bar.
  const startUploads = async (items) => {
    const target = dest.value.trim().replace(/^\/+|\/+$/g, "");
    const rows = items.map((it) => ({ ...it, path: (target ? target + "/" : "") + it.rel }));
    setHTML(queue, html`<div class="table-wrap"><table><tbody>${rows.map((r, i) => html`<tr>
      <td class="qname">${r.path}</td><td class="qbar">${bar(0, false, true)}</td>
      <td class="nowrap small dim" id="up-${i}">waiting</td></tr>`)}</tbody></table></div>`);
    const bars = queue.querySelectorAll(".bar > span");
    let failed = 0;
    for (const [i, r] of rows.entries()) {
      const label = document.getElementById("up-" + i);
      try {
        await uploadFile(r.file, {
          path: r.path,
          onProgress: (n) => {
            const p = pct(n, r.file.size || 1);
            bars[i].style.width = p.toFixed(1) + "%";
            label.textContent = `${p.toFixed(0)}% of ${bytes(r.file.size)}`;
          },
        });
        bars[i].style.width = "100%";
        label.textContent = "done";
      } catch (err) {
        failed++;
        label.textContent = err.message;
        bars[i].classList.add("err");
      }
    }
    toast(failed ? `${failed} upload(s) failed` : `Uploaded ${rows.length} file(s)`, failed ? "err" : "ok");
    folder = target;
    refresh();
  };
  document.getElementById("pick-files").addEventListener("change", (e) => {
    startUploads([...e.target.files].map((f) => ({ file: f, rel: f.name })));
    e.target.value = "";
  });
  document.getElementById("pick-folder").addEventListener("change", (e) => {
    startUploads([...e.target.files].map((f) => ({ file: f, rel: f.webkitRelativePath || f.name })));
    e.target.value = "";
  });

  await refresh();
  ctx.on("files", refresh);
}

