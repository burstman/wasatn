// WasaTN front-end glue.
//
// Two jobs only:
//   1. Attach the CSRF token to every non-GET HTMX request, so forms never need
//      to wire it up individually.
//   2. Clear HTMX-driven inputs after a successful swap where a form was
//      submitted.
(function () {
  "use strict";

  function csrfToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.getAttribute("content") : "";
  }

  document.body.addEventListener("htmx:configRequest", function (event) {
    var verb = event.detail.verb || "get";
    if (verb === "get" || verb === "head") {
      return;
    }
    var token = csrfToken();
    if (token) {
      event.detail.headers["X-CSRF-Token"] = token;
    }
  });

  // Give every destructive action a confirmation without inline handlers.
  document.body.addEventListener("htmx:beforeRequest", function (event) {
    var elt = event.detail.elt;
    if (!elt || !elt.dataset || !elt.dataset.confirm) {
      return;
    }
    if (!window.confirm(elt.dataset.confirm)) {
      event.preventDefault();
    }
  });

  // Alpine owns UI-only state; make sure HTMX swaps are re-scanned by Alpine so
  // newly inserted x-data components initialise.
  document.body.addEventListener("htmx:afterSwap", function (event) {
    if (window.Alpine) {
      window.Alpine.initTree(event.detail.target);
    }
  });
})();