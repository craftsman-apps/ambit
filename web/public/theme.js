// Apply the saved System/Light/Dark choice before the first paint, so there is no white flash.
try {
  var t = localStorage.getItem("ambit-theme");
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
} catch (e) {}
