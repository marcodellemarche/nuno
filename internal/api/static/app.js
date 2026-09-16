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

  // A budget, a share bar and the over-commit flag are all derived from the
  // ceilings, so editing one ceiling changes numbers that live elsewhere on
  // the card. The server sends the new values back and they are patched in
  // place, or the page would show a stale total until the next reload.
  function applyTotals(form, body) {
    var card = form.closest(".card");
    if (card && body.budget && body.used) {
      var totals = card.querySelector(".totals > span");
      if (totals) {
        totals.textContent = body.used + " of " + body.budget;
      }
      var totalFill = card.querySelector(".total-fill");
      if (totalFill && body.percent) {
        totalFill.style.width = body.percent + "%";
        totalFill.className = "total-fill " + (body.fill || "");
      }
    }

    // The edited row's own share bar, and its override tag: an override equal
    // to the tier's value changes nothing and must not be labelled.
    var row = form.closest("tr");
    if (row && body.row_percent) {
      var bar = row.querySelector(".bar");
      if (bar) {
        bar.title = body.row_percent + "%";
      }
      var rowFill = row.querySelector(".bar .fill");
      if (rowFill) {
        rowFill.style.width = body.row_percent + "%";
        rowFill.className = "fill " + (body.row_fill || "");
      }
    }
    if (row && body.override !== undefined) {
      var cell = form.parentElement;
      var pill = cell ? cell.querySelector(".override-pill") : null;
      if (body.override === "1" && !pill && cell) {
        pill = document.createElement("span");
        pill.className = "tier-pill override-pill";
        pill.title = "Set for this person only, above their tier";
        pill.textContent = "override";
        form.insertAdjacentElement("afterend", pill);
      } else if (body.override === "0" && pill) {
        pill.remove();
      }
    }

    var tier = form.closest(".tier-card");
    if (tier && body.budget) {
      var count = tier.querySelector(".member-count");
      if (count) {
        count.textContent = body.budget;
      }
    }
    if (tier && body.overcommit !== undefined) {
      var flag = tier.querySelector(".overcommit");
      if (body.overcommit === "1" && !flag) {
        var head = tier.querySelector(".tier-card-head");
        if (head) {
          flag = document.createElement("span");
          flag.className = "flag overcommit";
          flag.title = "Its percentages add up to more than 100, which over-commits deliberately";
          flag.textContent = "over-commits";
          head.appendChild(flag);
        }
      } else if (body.overcommit === "0" && flag) {
        flag.remove();
      }
    }
  }

  function enhanceEdit(form) {
    var input = form.querySelector("input.q");
    var confirm = form.querySelector("button[type=submit]");
    if (!input || !confirm) {
      return;
    }

    // The unit lives outside the input, in a wrapper that hides with it, so a
    // resting row does not show "50 GiB" and a stray "GiB" beside it.
    var unit = input.closest(".unit-field");

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
      if (unit) {
        unit.hidden = true;
      } else {
        input.hidden = true;
      }
      confirm.hidden = true;
      cancel.hidden = true;
      error.hidden = true;
    }

    function edit() {
      display.hidden = true;
      if (unit) {
        unit.hidden = false;
      } else {
        input.hidden = false;
      }
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
          applyTotals(form, body);
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
          if (form.dataset.onOk === "reload") {
            // A mapping moves a chip between cards, which only a reload can
            // draw. The pill is what the eye catches in the meantime.
            form.insertAdjacentElement("afterend", savedPill());
            window.setTimeout(function () {
              window.location.reload();
            }, 700);
            return;
          }
          window.location.reload();
        })
        .catch(function (failure) {
          window.location.search = "?err=" + encodeURIComponent(failure.message);
        });
    });
  }

  // The search filters the cards as the person types, so the list narrows
  // before Enter is ever pressed. The form still submits, which is the no-JS
  // path and the one that survives a reload.
  function enhanceSearch() {
    var input = document.querySelector("input[data-search-input]");
    if (!input) {
      return;
    }
    var cards = Array.prototype.slice.call(document.querySelectorAll(".card"));
    input.addEventListener("input", function () {
      var needle = input.value.trim().toLowerCase();
      cards.forEach(function (card) {
        var name = card.querySelector("h3");
        var hay = (name ? name.textContent : card.textContent) || "";
        card.hidden = needle !== "" && hay.toLowerCase().indexOf(needle) === -1;
      });
    });
  }

  // A checkbox that is also a filter submits itself, so turning it on is one
  // action rather than two.
  function enhanceAutoSubmit() {
    document.querySelectorAll("input[data-auto-submit]").forEach(function (box) {
      box.addEventListener("change", function () {
        if (box.form) {
          box.form.submit();
        }
      });
    });
  }

  document.querySelectorAll("form[data-edit]").forEach(enhanceEdit);
  document.querySelectorAll("form[data-fetch]").forEach(enhanceFetch);
  enhanceSearch();
  enhanceAutoSubmit();
})();
