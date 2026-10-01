// Applies a saved theme before the page renders (avoids a flash of the
// wrong theme). Loaded as a classic script in <head>.
try {
  var savedTheme = localStorage.getItem("theme");
  if (savedTheme === "light" || savedTheme === "dark") document.documentElement.dataset.theme = savedTheme;
} catch (e) { /* storage unavailable */ }
