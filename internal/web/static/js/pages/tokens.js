// Join tokens: create a code for new machines, list and revoke tokens.

import { api } from "../api.js";
import { html, setHTML, toast, confirmDialog, copyText, dateTime, coalesce } from "../util.js";

export async function render(main, _params, ctx) {
  setHTML(main, html`
    <div class="page-head"><h1>Join tokens</h1></div>
    <div class="grid cols-2">
      <form class="card stack" id="create">
        <h2>Add machines to the pool</h2>
        <div class="form-grid">
          <label class="field">Valid for
            <select name="expires">
              <option value="1h">1 hour</option><option value="24h" selected>24 hours</option>
              <option value="168h">7 days</option><option value="720h">30 days</option><option value="">Never expires</option>
            </select></label>
          <label class="field">Uses
            <select name="uses"><option value="1">1 machine</option><option value="5">5 machines</option>
              <option value="">Unlimited</option></select></label>
          <label class="field">New nodes
            <select name="approve"><option value="auto">Join immediately</option><option value="manual">Wait for my approval</option></select></label>
          <label class="field">Location label <input type="text" name="location" placeholder="home, vastai, gcp-us-central1"></label>
          <label class="field wide">Description <input type="text" name="description" placeholder="optional note"></label>
          <label class="check wide"><input type="checkbox" name="ephemeral"> Rented/cloud machines (ephemeral: removed automatically when gone)</label>
        </div>
        <div><button class="btn primary" type="submit">Create join code</button></div>
      </form>
      <div class="card" id="result">
        <h2>How joining works</h2>
        <p class="dim">Create a code, then on the new machine run the installer from a copy of the repository:</p>
        <div class="codebox"><code>sudo ./install.sh worker</code></div>
        <p class="dim">It finds this controller on the local network and asks for the code. The code also proves to the
        worker that it is talking to <i>this</i> controller, so there is nothing else to check.</p>
      </div>
    </div>
    <div class="card section"><h2>Tokens</h2><div id="list"></div></div>`);

  const form = document.getElementById("create");
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = form.elements;
    try {
      const res = await api.post("/join-tokens", {
        description: f.description.value.trim(),
        expires_in: f.expires.value,
        max_uses: f.uses.value ? Number(f.uses.value) : null,
        auto_approve: f.approve.value === "auto",
        location: f.location.value.trim(),
        ephemeral: f.ephemeral.checked,
      });
      showResult(res);
      refresh();
    } catch (err) {
      toast(err.message, "err");
    }
  });

  const list = document.getElementById("list");
  list.addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-revoke]");
    if (!btn) return;
    if (!await confirmDialog("Revoke this token?", "Machines that already joined keep working; the code can no longer be used.", "Revoke")) return;
    try {
      await api.del("/join-tokens/" + encodeURIComponent(btn.dataset.revoke));
      refresh();
    } catch (err) {
      toast(err.message, "err");
    }
  });

  const refresh = coalesce(async () => {
    const tokens = await api.get("/join-tokens");
    if (!tokens.length) {
      setHTML(list, html`<div class="empty">No tokens yet.</div>`);
      return;
    }
    const now = Date.now();
    setHTML(list, html`<div class="table-wrap"><table>
      <thead><tr><th>ID</th><th>State</th><th>Uses</th><th>New nodes</th><th>Location</th><th>Expires</th><th>Created</th><th>Description</th><th></th></tr></thead>
      <tbody>${tokens.map((t) => {
        const expired = t.expires_at && new Date(t.expires_at).getTime() < now;
        const used = t.max_uses && t.uses >= t.max_uses;
        const state = t.revoked_at ? html`<span class="badge">revoked</span>` : expired ? html`<span class="badge">expired</span>`
          : used ? html`<span class="badge">used up</span>` : html`<span class="badge ok">active</span>`;
        const active = !t.revoked_at && !expired && !used;
        return html`<tr>
          <td class="mono">${t.id}</td><td>${state}</td>
          <td>${t.uses}${t.max_uses ? " / " + t.max_uses : ""}</td>
          <td>${t.auto_approve ? "join immediately" : "need approval"}</td>
          <td>${t.location || "-"}${t.ephemeral ? html` <span class="badge info">ephemeral</span>` : ""}</td>
          <td class="nowrap">${t.expires_at ? dateTime(t.expires_at) : "never"}</td>
          <td class="nowrap faint">${dateTime(t.created_at)}</td>
          <td class="truncate">${t.description}</td>
          <td class="right">${active ? html`<button class="btn sm danger" data-revoke="${t.id}">Revoke</button>` : ""}</td>
        </tr>`;
      })}</tbody></table></div>`);
  });
  await refresh();
}

function showResult(res) {
  const box = document.getElementById("result");
  const cmd = (label, text) => html`<div class="small dim" style="margin-top:12px">${label}</div>
    <div class="codebox"><code>${text}</code><button class="btn sm" type="button" data-copy="${text}">Copy</button></div>`;
  setHTML(box, html`
    <h2>Join code</h2>
    <div class="row"><span class="joincode">${res.code}</span>
      <button class="btn sm" type="button" data-copy="${res.code}">Copy</button></div>
    <p class="hint">Shown only once. Typing it on the new machine is enough: case and dashes don't matter.</p>
    ${cmd("On the new machine, inside the repository (same network):", "sudo ./install.sh worker")}
    ${cmd("From another network:", `sudo ./install.sh worker --controller ${res.node_addr} --code ${res.code}`)}
    ${cmd("New machine without a copy of the repository yet:", res.commands.install)}
    ${cmd("Binary already installed (containers, supervisors):", res.commands.direct)}`);
  box.querySelectorAll("[data-copy]").forEach((b) => b.addEventListener("click", () => copyText(b.dataset.copy)));
}
