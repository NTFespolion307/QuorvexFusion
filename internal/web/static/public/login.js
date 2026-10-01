// Login page: exchanges the admin password for a session cookie.
(function () {
  try {
    const t = localStorage.getItem("theme");
    if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
  } catch (e) { /* storage unavailable */ }

  document.addEventListener("DOMContentLoaded", () => {
    const form = document.getElementById("login");
    const error = document.getElementById("error");
    const submit = document.getElementById("submit");
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      error.classList.add("hidden");
      submit.disabled = true;
      try {
        const res = await fetch("/api/v1/session", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password: document.getElementById("password").value }),
        });
        if (res.ok) {
          location.href = "/" + location.hash;
          return;
        }
        let msg = "Sign in failed";
        try { msg = (await res.json()).error || msg; } catch (e) { /* not JSON */ }
        error.textContent = msg;
        error.classList.remove("hidden");
      } catch (e) {
        error.textContent = "Cannot reach the controller";
        error.classList.remove("hidden");
      } finally {
        submit.disabled = false;
      }
    });
  });
})();
