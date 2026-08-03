// Upgrades the response-body textarea into a CodeMirror editor.
//
// It is an upgrade and not a replacement: the textarea is a real form field
// with a real name, and CodeMirror is asked to write back into it on every
// change. With JavaScript off, or if this file fails to load, the page is still
// a working form — which matters, because this is the only editor in the
// application and losing it would mean losing the ability to define an endpoint
// at all.
(function () {
  "use strict";

  function upgrade(textarea) {
    if (typeof window.CodeMirror !== "function") {
      return null;
    }

    var editor = window.CodeMirror.fromTextArea(textarea, {
      // The JSON dialect of the JavaScript mode: no statements, no keywords,
      // just the value grammar.
      mode: { name: "javascript", json: true },
      lineNumbers: true,
      lineWrapping: true,
      autoCloseBrackets: true,
      matchBrackets: true,
      tabSize: 2,
      indentUnit: 2,
      viewportMargin: Infinity,
      extraKeys: {
        // Tab indents rather than trapping focus: a keyboard user has to be
        // able to leave the editor and reach the submit button.
        Tab: function (cm) {
          cm.execCommand("insertSoftTab");
        },
        "Shift-Tab": function (cm) {
          cm.execCommand("indentLess");
        },
        Escape: function (cm) {
          cm.getInputField().blur();
        },
      },
    });

    // fromTextArea only syncs on form submit. Syncing on every change means
    // anything else reading the field — the Format button below — sees what is
    // on screen.
    editor.on("change", function () {
      editor.save();
    });

    return editor;
  }

  // format rewrites the body as indented JSON, leaving it alone if it is not
  // JSON at all. A response body does not have to be JSON, so failing to parse
  // is an ordinary answer and not an error worth interrupting anyone about.
  function format(editor, textarea, button) {
    var source = editor ? editor.getValue() : textarea.value;
    var parsed;
    try {
      parsed = JSON.parse(source);
    } catch (err) {
      var original = button.textContent;
      button.textContent = "Not JSON";
      setTimeout(function () {
        button.textContent = original;
      }, 1500);
      return;
    }

    var pretty = JSON.stringify(parsed, null, 2);
    if (editor) {
      editor.setValue(pretty);
    } else {
      textarea.value = pretty;
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    var editors = {};

    document.querySelectorAll("textarea[data-editor]").forEach(function (textarea) {
      var editor = upgrade(textarea);
      if (editor && textarea.id) {
        editors[textarea.id] = editor;
      }
    });

    document.querySelectorAll("[data-format-json]").forEach(function (button) {
      button.addEventListener("click", function () {
        var id = button.getAttribute("data-format-json");
        var textarea = document.getElementById(id);
        if (textarea) {
          format(editors[id], textarea, button);
        }
      });
    });
  });
})();
