// REST API access and the live event stream.

export class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function request(method, path, body) {
  const opts = { method, credentials: "same-origin", headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/api/v1" + path, opts);
  if (res.status === 401) {
    location.href = "/login" + location.hash;
    throw new APIError(401, "not logged in");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch (e) { /* not JSON */ }
    throw new APIError(res.status, msg);
  }
  if (res.status === 204) return null;
  return (res.headers.get("Content-Type") || "").includes("json") ? res.json() : res;
}

export const api = {
  get: (p) => request("GET", p),
  post: (p, body) => request("POST", p, body ?? {}),
  put: (p, body) => request("PUT", p, body ?? {}),
  del: (p) => request("DELETE", p),
  // Raw response (for logs: body text plus X-Log-* headers).
  raw: (p) => request("GET", p),
};

// --- live events ---------------------------------------------------------
// The server sends topic names ("nodes", "jobs", "metrics") whenever
// something changes; pages subscribe and refetch what they show.

const handlers = new Map(); // topic -> Set(fn)
let source = null;

export function onEvent(topic, fn) {
  if (!handlers.has(topic)) handlers.set(topic, new Set());
  handlers.get(topic).add(fn);
  return () => handlers.get(topic).delete(fn);
}

export function connectEvents(onStatus) {
  if (source) return;
  source = new EventSource("/api/v1/events");
  source.onopen = () => onStatus(true);
  source.onerror = () => onStatus(false); // the browser reconnects by itself
  for (const topic of ["nodes", "jobs", "metrics"]) {
    source.addEventListener(topic, () => handlers.get(topic)?.forEach((fn) => fn()));
  }
}
