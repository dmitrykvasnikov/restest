// Appends the request log as it arrives, over server-sent events.
//
// The rows come down as rendered HTML rather than as JSON, so that a row which
// arrives live and one which arrives on a refresh are the same markup — they
// are the same template, executed on the server (internal/web/log.go).
//
// Without this file the page still works: it shows the entries that existed
// when it was loaded, and reloading shows the ones since. This is the "without
// a refresh" half of the milestone, not the mechanism.
(function () {
  "use strict";

  // How many rows are kept in the page. A tail left open against a busy project
  // would otherwise grow until the tab does; the older ones are a reload away.
  var MAX_ROWS = 200;

  function status(state, text) {
    var badge = document.getElementById("log-status");
    if (!badge) {
      return;
    }
    badge.setAttribute("data-state", state);
    badge.textContent = text;
  }

  function prepend(tbody, html) {
    var empty = document.getElementById("log-empty");
    if (empty) {
      empty.remove();
    }

    // insertAdjacentHTML on a tbody parses a <tr> in table context, which
    // innerHTML on a detached element would not.
    tbody.insertAdjacentHTML("afterbegin", html);

    var rows = tbody.querySelectorAll("tr.log-row");
    for (var i = MAX_ROWS; i < rows.length; i++) {
      rows[i].remove();
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    var tbody = document.getElementById("log-rows");
    if (!tbody || typeof EventSource === "undefined") {
      return;
    }

    var url = tbody.getAttribute("data-stream");
    var after = tbody.getAttribute("data-after");
    if (after) {
      // Where the rendered page ends, so nothing recorded between the page
      // being built and this connection opening is skipped.
      url += "?after=" + encodeURIComponent(after);
    }

    var source = new EventSource(url);

    source.addEventListener("open", function () {
      status("live", "Live");
    });

    source.addEventListener("exchange", function (event) {
      prepend(tbody, event.data);
    });

    // EventSource reconnects on its own, including when the server ends a
    // connection at its lifetime; this only says so while it is not connected.
    source.addEventListener("error", function () {
      status("reconnecting", "Reconnecting…");
    });
  });
})();
