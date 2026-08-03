// Shows one half of a form and hides the other, according to a radio group.
//
// The endpoint form describes two different things — a static response and a
// collection — and only one set of fields applies at a time. Without this file
// both sets are shown and both are submitted, and the form still works: the
// server reads the fields belonging to the kind that was chosen and ignores the
// rest. This is the polish, not the mechanism.
//
// Hidden fields are disabled rather than merely hidden. A hidden input that is
// still `required` stops the browser submitting the form and gives no way to see
// why, because there is nothing on screen to point at.
(function () {
  "use strict";

  function apply(group) {
    var name = group.getAttribute("data-kind-toggle");
    var chosen = group.querySelector("input[name='" + name + "']:checked");
    if (!chosen) {
      return;
    }

    document.querySelectorAll("[data-kind]").forEach(function (section) {
      var active = section.getAttribute("data-kind") === chosen.value;
      section.hidden = !active;

      section.querySelectorAll("input, select, textarea").forEach(function (field) {
        field.disabled = !active;
      });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-kind-toggle]").forEach(function (group) {
      group.addEventListener("change", function () {
        apply(group);
      });
      apply(group);
    });
  });
})();
