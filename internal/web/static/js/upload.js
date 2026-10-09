// Resumable file uploads from the browser.
//
// A file is sent in 8 MB chunks to an upload session on the controller.
// After a network error the uploader asks the controller how much arrived
// and continues from there. The session ID is remembered (per file name,
// size and date) so that picking the same file again after a page reload
// or a lost connection resumes instead of starting over.

const CHUNK = 8 << 20;

function remember(key, id) {
  try { if (id) localStorage.setItem(key, id); else localStorage.removeItem(key); } catch (e) { /* storage unavailable */ }
}

function recall(key) {
  try { return localStorage.getItem(key); } catch (e) { return null; }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

class UploadError extends Error {
  constructor(status, message, offset) { super(message); this.status = status; this.offset = offset; }
}

// put sends one chunk with progress (fetch can't report upload progress).
function put(url, body, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (xhr.status === 401) { location.href = "/login"; return; }
      let res = {};
      try { res = JSON.parse(xhr.responseText); } catch (e) { /* not JSON */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(res);
      else reject(new UploadError(xhr.status, res.error || `upload failed (${xhr.status})`, res.offset));
    };
    xhr.onerror = () => reject(new UploadError(0, "network error"));
    xhr.send(body);
  });
}

async function json(method, url, body) {
  const res = await fetch(url, {
    method, credentials: "same-origin",
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  let data = {};
  try { data = await res.json(); } catch (e) { /* empty */ }
  if (!res.ok) throw new UploadError(res.status, data.error || res.statusText, data.offset);
  return data;
}

async function serverOffset(id) {
  const res = await fetch(`/api/v1/uploads/${id}`, { method: "HEAD", credentials: "same-origin" });
  return res.ok ? Number(res.headers.get("X-Upload-Offset")) : null;
}

// uploadFile uploads a File and returns {sha256, size} (plus {file} when a
// library path is given). onProgress(bytesDone) is called as it goes.
export async function uploadFile(file, { path = "", onProgress = () => {} } = {}) {
  const key = `upload:${file.name}:${file.size}:${file.lastModified}:${path}`;
  let id = recall(key);
  let offset = id ? await serverOffset(id) : null;
  if (offset === null) {
    id = (await json("POST", "/api/v1/uploads")).id;
    remember(key, id);
    offset = 0;
  }
  onProgress(offset);

  let failures = 0;
  while (offset < file.size) {
    const end = Math.min(offset + CHUNK, file.size);
    try {
      const res = await put(`/api/v1/uploads/${id}?offset=${offset}`, file.slice(offset, end), (n) => onProgress(offset + n));
      offset = res.offset;
      failures = 0;
    } catch (err) {
      if (err.status === 409 && typeof err.offset === "number") { offset = err.offset; continue; }
      if (err.status === 404) { remember(key, null); throw new Error(`${file.name}: the upload expired; please try again`); }
      if (++failures > 10) throw new Error(`${file.name}: ${err.message}; pick the file again to resume`);
      await sleep(Math.min(1000 * 2 ** failures, 30000)); // the connection may come back
      const o = await serverOffset(id).catch(() => null);
      if (o !== null) offset = o;
    }
    onProgress(offset);
  }
  const result = await json("POST", `/api/v1/uploads/${id}/finish`, { size: file.size, path });
  remember(key, null);
  return result;
}
