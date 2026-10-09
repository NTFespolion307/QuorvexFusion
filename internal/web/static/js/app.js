// App shell: navigation, hash routing, theme switch, live-update status.
//
// Each page module exports render(main, params, ctx). ctx.on(topic, fn)
// subscribes to live events and ctx.cleanup(fn) registers teardown; both
// are undone automatically when the user navigates away.

import { api, connectEvents, onEvent } from "./api.js";
import { html, setHTML, toast, coalesce } from "./util.js";
import * as dashboard from "./pages/dashboard.js";
import * as nodes from "./pages/nodes.js";
import * as node from "./pages/node.js";
import * as tokens from "./pages/tokens.js";
import * as jobs from "./pages/jobs.js";
import * as job from "./pages/job.js";
import * as task from "./pages/task.js";
import * as settings from "./pages/settings.js";
import * as files from "./pages/files.js";

const icons = {
  dashboard: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/></svg>`,
  nodes: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01M7 17h.01"/></svg>`,
  jobs: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 6h16M4 12h16M4 18h10"/></svg>`,
  files: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>`,
  tokens: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="8" cy="15" r="4"/><path d="m11 12 9-9m-4 4 3 3"/></svg>`,
  settings: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/></svg>`,
};

const NAV = [
  { path: "", label: "Dashboard", icon: icons.dashboard },
  { path: "nodes", label: "Nodes", icon: icons.nodes, id: "nav-nodes" },
  { path: "jobs", label: "Jobs", icon: icons.jobs },
  { path: "files", label: "Files", icon: icons.files },
  { path: "tokens", label: "Join tokens", icon: icons.tokens },
  { path: "settings", label: "Settings", icon: icons.settings },
];

// [pattern, page]; patterns are matched against the hash path.
const ROUTES = [
  [/^$/, dashboard],
  [/^nodes$/, nodes],
  [/^nodes\/([^/]+)$/, node],
  [/^jobs$/, jobs],
  [/^jobs\/([^/]+)$/, job],
  [/^tasks\/([^/]+)$/, task],
  [/^tokens$/, tokens],
  [/^files$/, files],
  [/^settings$/, settings],
];

let teardown = [];

function renderNav(current) {
  const section = current.split("/")[0].replace(/^tasks$/, "jobs");
  setHTML(document.getElementById("nav"), NAV.map((n) =>
    html`<a href="#/${n.path}" class="${section === n.path ? "active" : ""}">${n.icon}<span>${n.label}</span>${
      n.id ? html`<span class="count hidden" id="${n.id}"></span>` : ""}</a>`));
  updatePendingCount();
}

// The sidebar shows how many nodes await approval.
const updatePendingCount = coalesce(async () => {
  const s = await api.get("/status");
  const el = document.getElementById("nav-nodes");
  if (!el) return;
  el.textContent = s.nodes_pending;
  el.title = `${s.nodes_pending} node(s) waiting for approval`;
  el.classList.toggle("hidden", !s.nodes_pending);
});

async function route() {
  teardown.forEach((fn) => { try { fn(); } catch (e) { console.error(e); } });
  teardown = [];
  const path = decodeURIComponent(location.hash.replace(/^#\/?/, "")).replace(/\/$/, "");
  renderNav(path);
  const main = document.getElementById("main");
  for (const [re, page] of ROUTES) {
    const m = path.match(re);
    if (!m) continue;
    const ctx = {
      on: (topic, fn) => teardown.push(onEvent(topic, fn)),
      cleanup: (fn) => teardown.push(fn),
    };
    setHTML(main, html`<div class="dim">Loading…</div>`);
    try {
      await page.render(main, m.slice(1), ctx);
    } catch (e) {
      setHTML(main, html`<div class="card empty">Could not load this page: ${e.message}</div>`);
    }
    window.scrollTo(0, 0);
    return;
  }
  setHTML(main, html`<div class="card empty">Page not found. <a href="#/">Back to the dashboard</a></div>`);
}

// --- theme -----------------------------------------------------------------

function currentTheme() {
  const set = document.documentElement.dataset.theme;
  if (set) return set;
  return matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function setupTheme() {
  const btn = document.getElementById("theme-toggle");
  const label = () => { btn.textContent = currentTheme() === "dark" ? "Light theme" : "Dark theme"; };
  label();
  btn.addEventListener("click", () => {
    const next = currentTheme() === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem("theme", next); } catch (e) { /* storage unavailable */ }
    label();
    route(); // re-render so charts pick up the new colors
  });
}

// --- startup -----------------------------------------------------------------

function start() {
  setupTheme();
  document.getElementById("logout").addEventListener("click", async () => {
    await api.del("/session").catch(() => {});
    location.href = "/login";
  });
  connectEvents((up) => {
    document.getElementById("live").classList.toggle("on", up);
    document.getElementById("live-text").textContent = up ? "live" : "reconnecting";
  });
  onEvent("nodes", updatePendingCount);
  // Clickable rows/cards carry data-href (inline onclick is blocked by the
  // Content-Security-Policy). Real links and buttons inside keep working.
  document.addEventListener("click", (e) => {
    const el = e.target.closest("[data-href]");
    if (el && !e.target.closest("a, button, input, select, textarea, label")) location.hash = el.dataset.href;
  });
  window.addEventListener("hashchange", route);
  window.addEventListener("unhandledrejection", (e) => {
    if (e.reason?.status !== 401) toast(e.reason?.message || String(e.reason), "err");
  });
  route();
}

// uPlot is a deferred classic script; modules run after it, but be safe.
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
else start();
