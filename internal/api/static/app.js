// SPDX-License-Identifier: AGPL-3.0-or-later
//
// The only JavaScript in Nuno, and it is an enhancement, not a path of its
// own: every control it touches is a real form that posts to the same action
// route and reloads the page when this file never runs (ADR-0030).

(function () {
  "use strict";

  var PENCIL =
    '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"></path><path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4Z"></path></svg>';
  var CHECK =
    '<svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"></path></svg>';
  var CROSS =
    '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18M6 6l12 12"></path></svg>';

  // post sends the form to its own action, saying who is asking so the handler
  // answers with a value to show instead of a redirect to follow.
  function post(form) {
    return fetch(form.action, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "X-Requested-With": "nuno-inline-edit",
      },
      body: new URLSearchParams(new FormData(form)).toString(),
      credentials: "same-origin",
    }).then(function (response) {
      return response
        .json()
        .catch(function () {
          return {};
        })
        .then(function (body) {
          if (!response.ok) {
            throw new Error(body.err || "that did not save");
          }
          return body;
        });
    });
  }

  function savedPill() {
    var pill = document.createElement("span");
    pill.className = "saved-pill";
    pill.innerHTML = CHECK + " saved";
    pill.addEventListener("animationend", function () {
      pill.remove();
    });
    return pill;
  }

  function enhanceEdit(form) {
    var input = form.querySelector("input.q");
    var confirm = form.querySelector("button[type=submit]");
    if (!input || !confirm) {
      return;
    }

    var display = document.createElement("button");
    display.type = "button";
    display.className = "edit";
    var cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "icon-btn";
    cancel.title = "Cancel";
    cancel.innerHTML = CROSS;
    confirm.insertAdjacentElement("afterend", cancel);
    form.insertAdjacentElement("afterbegin", display);

    var error = document.createElement("span");
    error.className = "edit-error";
    error.hidden = true;
    form.appendChild(error);

    function show(text) {
      display.innerHTML = "";
      display.appendChild(document.createTextNode(text + " "));
      display.insertAdjacentHTML("beforeend", PENCIL);
    }

    function rest() {
      display.hidden = false;
      input.hidden = true;
      confirm.hidden = true;
      cancel.hidden = true;
      error.hidden = true;
    }

    function edit() {
      display.hidden = true;
      input.hidden = false;
      confirm.hidden = false;
      cancel.hidden = false;
      input.focus();
      input.select();
    }

    var committed = input.value;
    show(form.dataset.display || input.value);
    rest();

    display.addEventListener("click", edit);
    cancel.addEventListener("click", function () {
      input.value = committed;
      rest();
    });
    input.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        event.preventDefault();
        input.value = committed;
        rest();
      }
    });

    form.addEventListener("submit", function (event) {
      event.preventDefault();
      confirm.disabled = true;
      error.hidden = true;
      post(form)
        .then(function (body) {
          committed = input.value;
          show(body.value || input.value || "—");
          rest();
          display.insertAdjacentElement("afterend", savedPill());
        })
        .catch(function (failure) {
          // The typed value stays where it is, so nothing has to be retyped.
          error.textContent = failure.message;
          error.hidden = false;
        })
        .then(function () {
          confirm.disabled = false;
        });
    });
  }

  // A chip is not a value being edited, so it gets the same instant feedback
  // without the two-state dance: it either disappears or the page catches up.
  function enhanceFetch(form) {
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      post(form)
        .then(function () {
          if (form.dataset.onOk === "remove") {
            var chip = form.closest("[data-chip]");
            if (chip) {
              chip.replaceWith(savedPill());
              return;
            }
          }
          window.location.reload();
        })
        .catch(function (failure) {
          window.location.search = "?err=" + encodeURIComponent(failure.message);
        });
    });
  }

  document.querySelectorAll("form[data-edit]").forEach(enhanceEdit);
  document.querySelectorAll("form[data-fetch]").forEach(enhanceFetch);
})();
