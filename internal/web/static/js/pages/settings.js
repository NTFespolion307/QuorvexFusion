// Settings: controller info, admin password, API tokens.

import { api } from "../api.js";
import { html, setHTML, toast, confirmDialog, modal, copyText, ago, dateTime, coalesce } from "../util.js";

export async function render(main, _params, ctx) {
  const info = await api.get("/info");
  setHTML(main, html`
    <div class="page-head"><h1>Settings</h1></div>
    <div class="grid cols-2">
      <div class="card"><h2>Controller</h2>
        <dl class="kv">
          <dt>Version</dt><dd>${info.version}</dd>
          <dt>Web UI</dt><dd class="mono">${info.ui_url}</dd>
          <dt>Worker address</dt><dd class="mono">${info.node_addr}</dd>
          <dt>CA fingerprint</dt><dd class="mono small">${info.ca_fingerprint}</dd>
        </dl>
        <p class="hint">Browsers warn about the certificate because it comes from the cluster's own CA.
        To remove the warning, import the CA certificate into your browser or OS:</p>
        <button class="btn sm" id="dl-ca">Download CA certificate</button>
      </div>
      <form class="card stack" id="password">
        <h2>Admin password</h2>
        <label class="field">Current password <input type="password" name="current" autocomplete="current-password" required></label>
        <label class="field">New password (min 8 characters) <input type="password" name="next" autocomplete="new-password" minlength="8" required></label>
        <label class="field">Repeat new password <input type="password" name="again" autocomplete="new-password" required></label>
        <div><button class="btn primary" type="submit">Change password</button></div>
        <p class="hint">All browser sessions are signed out. API tokens keep working.</p>
      </form>
    </div>
    <div class="card section">
      <div class="row" style="margin-bottom:10px"><h2 style="margin:0">API tokens</h2><span style="flex:1"></span>
        <button class="btn" id="new-token">New API token</button></div>
      <p class="hint">For the CLI on other computers and for scripts: <code>CLUSTER_CONTROLLER=${info.ui_url} CLUSTER_TOKEN=… cluster status</code>
        (or run <code>cluster login</code>, which creates a token for you).</p>
      <div id="tokens"></div>
    </div>`);

  document.getElementById("dl-ca").addEventListener("click", () => {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([info.ca_cert], { type: "application/x-pem-file" }));
    a.download = "cluster-ca.crt";
    a.click();
  });

  const pw = document.getElementById("password");
  pw.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = pw.elements;
    if (f.next.value !== f.again.value) { toast("The new passwords differ", "err"); return; }
    try {
      await api.post("/password", { current: f.current.value, new: f.next.value });
      toast("Password changed; please sign in again", "ok");
      setTimeout(() => { location.href = "/login"; }, 1200);
    } catch (err) {
      toast(err.message, "err");
    }
  });

  const tokensEl = document.getElementById("tokens");
  const refresh = coalesce(async () => {
    const toks = await api.get("/api-tokens");
    setHTML(tokensEl, toks.length ? html`<div class="table-wrap"><table>
      <thead><tr><th>ID</th><th>Name</th><th>Created</th><th>Last used</th><th></th></tr></thead>
      <tbody>${toks.map((t) => html`<tr><td class="mono">${t.id}</td><td>${t.name}</td>
        <td class="faint">${dateTime(t.created_at)}</td><td>${ago(t.last_used)}</td>
        <td class="right"><button class="btn sm danger" data-revoke="${t.id}">Revoke</button></td></tr>`)}</tbody>
    </table></div>` : html`<div class="empty">No API tokens.</div>`);
  });
  tokensEl.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-revoke]");
    if (!b || !await confirmDialog("Revoke this API token?", "Anything using it stops working immediately.", "Revoke")) return;
    await api.del("/api-tokens/" + encodeURIComponent(b.dataset.revoke)).catch((err) => toast(err.message, "err"));
    refresh();
  });
  document.getElementById("new-token").addEventListener("click", async () => {
    const form = await modal("New API token", html`<label class="field">Name
      <input type="text" name="name" required placeholder="laptop, ci, backup-script"></label>`, { okText: "Create" });
    if (!form) return;
    try {
      const res = await api.post("/api-tokens", { name: form.name.value.trim() });
      await modal("API token created", html`<p class="dim">Copy it now; it is not shown again.</p>
        <div class="codebox"><code>${res.token}</code><button type="button" class="btn sm" id="copy-tok">Copy</button></div>`,
        { okText: "Done", cancelText: "" }).catch(() => {});
      refresh();
    } catch (err) {
      toast(err.message, "err");
    }
  });
  // The token dialog lives outside main, so listen on the document.
  const onCopy = (e) => {
    if (e.target.id === "copy-tok") copyText(e.target.previousElementSibling.textContent);
  };
  document.addEventListener("click", onCopy);
  ctx.cleanup(() => document.removeEventListener("click", onCopy));
  await refresh();
}
