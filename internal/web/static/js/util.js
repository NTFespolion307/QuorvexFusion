// Small helpers shared by all pages: safe HTML templating, formatting,
// toasts and modals.

// --- HTML templating ---------------------------------------------------
// html`<b>${value}</b>` escapes every interpolated value unless it is
// itself the result of html`` (or raw()). Arrays are joined. This keeps
// user-controlled strings (hostnames, commands, labels) from injecting markup.

class SafeHTML {
  constructor(s) { this.s = s; }
  toString() { return this.s; }
}

export function raw(s) { return new SafeHTML(String(s)); }

export function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function part(v) {
  if (v instanceof SafeHTML) return v.s;
  if (Array.isArray(v)) return v.map(part).join("");
  if (v === null || v === undefined || v === false) return "";
  return esc(v);
}

export function html(strings, ...values) {
  let out = strings[0];
  values.forEach((v, i) => { out += part(v) + strings[i + 1]; });
  return new SafeHTML(out);
}

// setHTML accepts html`` results and arrays of them.
export function setHTML(el, content) { el.innerHTML = part(content); }

// --- formatting ----------------------------------------------------------

export function bytes(n) {
  n = Number(n || 0);
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n < 10 ? n.toFixed(1) : n.toFixed(0)) + " " + units[i];
}

export function num(n, digits = 1) {
  n = Number(n || 0);
  return Number.isInteger(n) ? String(n) : n.toFixed(digits);
}

export function pct(part, total) { return total > 0 ? Math.min(100, (100 * part) / total) : 0; }

export function duration(ms) {
  if (ms === null || ms === undefined || ms < 0) return "-";
  const s = Math.floor(ms / 1000);
  if (s < 60) return ms < 10000 ? (ms / 1000).toFixed(1) + "s" : s + "s";
  const m = Math.floor(s / 60), h = Math.floor(m / 60), d = Math.floor(h / 24);
  if (d > 0) return `${d}d ${h % 24}h`;
  if (h > 0) return `${h}h ${m % 60}m`;
  return `${m}m ${s % 60}s`;
}

export function ago(ts) {
  if (!ts) return "never";
  const ms = Date.now() - new Date(ts).getTime();
  return ms < 5000 ? "just now" : duration(ms) + " ago";
}

export function since(start, end) {
  if (!start) return "-";
  return duration((end ? new Date(end) : new Date()) - new Date(start));
}

export function dateTime(ts) { return ts ? new Date(ts).toLocaleString() : "-"; }

// Usage bar colored by severity (fuller = warmer), or "progress" bar in
// the success color.
export function bar(percent, thick = false, progress = false) {
  const p = Math.max(0, Math.min(100, percent || 0));
  const cls = progress ? "ok" : p >= 90 ? "err" : p >= 70 ? "warn" : "";
  return html`<div class="bar${thick ? " thick" : ""}"><span class="${cls}" style="width:${p.toFixed(1)}%"></span></div>`;
}

export function meter(label, percent, valueText) {
  return html`<div class="meter"><span class="k">${label}</span>${bar(percent)}<span class="v">${valueText}</span></div>`;
}

const STATE_CLASS = {
  online: "ok", succeeded: "ok", running: "accent", assigned: "accent", queued: "info",
  pending: "warn", offline: "", failed: "err", canceled: "", revoked: "err", draining: "warn",
};

export function stateBadge(state) {
  return html`<span class="badge dot ${STATE_CLASS[state] ?? ""}">${state}</span>`;
}

// --- toasts and dialogs -------------------------------------------------

export function toast(message, kind = "") {
  const box = document.getElementById("toasts");
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = message;
  box.appendChild(el);
  setTimeout(() => el.remove(), kind === "err" ? 7000 : 3500);
}

// modal(title, bodyHTML, {okText, danger}) resolves to the dialog element
// when confirmed, or null when cancelled.
export function modal(title, body, { okText = "OK", danger = false, cancelText = "Cancel" } = {}) {
  return new Promise((resolve) => {
    const overlay = document.createElement("div");
    overlay.className = "overlay";
    setHTML(overlay, html`<form class="card modal">
      <h2>${title}</h2>
      <div>${body}</div>
      <div class="foot">
        ${cancelText ? html`<button type="button" class="btn ghost" data-cancel>${cancelText}</button>` : ""}
        <button type="submit" class="btn ${danger ? "danger" : "primary"}">${okText}</button>
      </div>
    </form>`);
    const form = overlay.querySelector("form");
    const close = (v) => { overlay.remove(); document.removeEventListener("keydown", onKey); resolve(v); };
    const onKey = (e) => { if (e.key === "Escape") close(null); };
    form.addEventListener("submit", (e) => { e.preventDefault(); close(form); });
    overlay.querySelector("[data-cancel]")?.addEventListener("click", () => close(null));
    overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) close(null); });
    document.addEventListener("keydown", onKey);
    document.body.appendChild(overlay);
    form.querySelector("input, textarea, select, button[type=submit]")?.focus();
  });
}

export function confirmDialog(title, message, okText = "Confirm", danger = true) {
  return modal(title, html`<p class="dim">${message}</p>`, { okText, danger }).then((r) => r !== null);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied to clipboard", "ok");
  } catch (e) {
    toast("Copy failed: select the text and copy it manually", "err");
  }
}

// Runs fn at most once at a time; calls arriving meanwhile cause one rerun.
export function coalesce(fn) {
  let running = false, again = false;
  const run = async () => {
    if (running) { again = true; return; }
    running = true;
    try { await fn(); } catch (e) { console.error(e); }
    running = false;
    if (again) { again = false; run(); }
  };
  return run;
}

// Parses "k=v, k2=v2" (or one per line) into an object.
export function parseKV(text) {
  const out = {};
  for (const part of String(text || "").split(/[,\n]/)) {
    const s = part.trim();
    if (!s) continue;
    const i = s.indexOf("=");
    if (i <= 0) throw new Error(`"${s}" is not KEY=VALUE`);
    out[s.slice(0, i).trim()] = s.slice(i + 1).trim();
  }
  return out;
}

export function kvText(obj, sep = ", ") {
  return Object.entries(obj || {}).map(([k, v]) => `${k}=${v}`).join(sep);
}
