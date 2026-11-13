// Runway portal. The server renders everything; this file only keeps the
// clocks moving and asks for a fresh panel when the cluster says something
// changed.
(function () {
  "use strict";

  function pad(n) { return String(n).padStart(2, "0"); }

  function remainingText(ms) {
    if (ms <= 0) return "Time is up";
    var s = Math.floor(ms / 1000);
    var d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
    if (d > 0) return d + "d " + pad(h) + "h " + pad(m) + "m";
    if (h > 0) return h + "h " + pad(m) + "m " + pad(sec) + "s";
    return m + "m " + pad(sec) + "s";
  }

  function clockText(iso) {
    var t = new Date(iso), now = new Date();
    var time = t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    if (t.toDateString() === now.toDateString()) return "at " + time;
    return t.toLocaleDateString([], { weekday: "short", day: "numeric", month: "short" }) + " at " + time;
  }

  function tick() {
    var now = Date.now();
    document.querySelectorAll(".env").forEach(function (env) {
      var created = Date.parse(env.dataset.created);
      var expires = Date.parse(env.dataset.expires || "");
      var label = env.querySelector("[data-remaining]");
      var strip = env.querySelector(".strip");
      if (isNaN(expires)) { return; }
      var used = Math.min(1, Math.max(0, (now - created) / (expires - created)));
      strip.style.setProperty("--used", (used * 100).toFixed(2) + "%");
      if (env.classList.contains("ending")) { return; }
      label.textContent = remainingText(expires - now) + (expires > now ? " left" : "");
      label.classList.toggle("soon", expires - now < 5 * 60 * 1000);
    });
    document.querySelectorAll("time[data-clock]").forEach(function (el) {
      el.textContent = clockText(el.dataset.clock);
      el.setAttribute("datetime", el.dataset.clock);
    });
  }

  function refreshPanel() {
    var panel = document.getElementById("panel");
    if (panel && window.htmx) { window.htmx.trigger(panel, "refresh"); }
  }

  // One event per change in the cluster; several can arrive together, so
  // wait a moment and refresh once.
  var pending = null;
  function connect() {
    if (!document.getElementById("panel") || !window.EventSource) { return; }
    var source = new EventSource("/events");
    source.addEventListener("change", function () {
      clearTimeout(pending);
      pending = setTimeout(refreshPanel, 150);
    });
    // EventSource reconnects by itself; nothing to do on error.
  }

  document.addEventListener("DOMContentLoaded", function () {
    tick();
    setInterval(tick, 1000);
    connect();

    var name = document.getElementById("name"), echo = document.getElementById("name-echo");
    if (name && echo) {
      name.addEventListener("input", function () { echo.textContent = name.value || "name"; });
    }
  });

  // Fresh HTML arrived: fill in its clocks straight away.
  document.addEventListener("htmx:afterSwap", tick);

  // Signed out (or the session ended): go back through the front door.
  document.addEventListener("htmx:responseError", function (event) {
    if (event.detail.xhr && event.detail.xhr.status === 401) { window.location.reload(); }
  });

  document.addEventListener("click", function (event) {
    var button = event.target.closest("[data-copy]");
    if (!button || !navigator.clipboard) { return; }
    navigator.clipboard.writeText(button.dataset.copy).then(function () {
      var old = button.textContent;
      button.textContent = "Copied";
      setTimeout(function () { button.textContent = old; }, 1500);
    });
  });
})();
