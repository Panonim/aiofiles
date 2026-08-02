(function () {
  var TYPEAHEAD_MS = 700;
  var POP_MAX_H = 288;
  var POP_GAP = 4;
  var EDGE = 8;

  var seq = 0;
  var instances = [];
  var openInst = null;
  var fineMq = null;
  var docObs = null;
  var scanQueued = false;
  var globalsBound = false;
  var started = false;

  /* Captured once so a patched element can still reach the real behaviour. */
  var SELECT_INDEX = descriptor(window.HTMLSelectElement, "selectedIndex");
  var SELECT_VALUE = descriptor(window.HTMLSelectElement, "value");
  var OPTION_SELECTED = descriptor(window.HTMLOptionElement, "selected");

  function descriptor(ctor, prop) {
    if (!ctor || !ctor.prototype) return null;
    return Object.getOwnPropertyDescriptor(ctor.prototype, prop);
  }

  function eligible(sel) {
    if (sel.xsel) return false;
    if (sel.multiple || sel.size > 1) return false;
    /* The opt-out: data-native keeps the platform control everywhere. */
    if (sel.hasAttribute("data-native")) return false;
    return true;
  }

  function enhance(sel) {
    var inst = {
      select: sel,
      id: (sel.id || "xsel-" + ++seq) + "-xsel",
      wrap: null,
      btn: null,
      valueEl: null,
      pop: null,
      rows: [],
      opts: [],
      active: -1,
      open: false,
      typed: "",
      typedAt: 0,
      obs: null,
      form: null,
      patched: [],
      queued: false,
      needRender: true,
      alive: true,
    };

    var wrap = document.createElement("div");
    wrap.className = "xsel";
    sel.parentNode.insertBefore(wrap, sel);
    wrap.appendChild(sel);

    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "xsel-btn";
    btn.id = inst.id + "-btn";
    btn.setAttribute("role", "combobox");
    btn.setAttribute("aria-haspopup", "listbox");
    btn.setAttribute("aria-expanded", "false");
    btn.setAttribute("aria-controls", inst.id + "-list");

    var value = document.createElement("span");
    value.className = "xsel-value";
    var arrow = document.createElement("span");
    arrow.className = "xsel-arrow";
    arrow.setAttribute("aria-hidden", "true");
    btn.appendChild(value);
    btn.appendChild(arrow);
    wrap.appendChild(btn);

    var pop = document.createElement("div");
    pop.className = "xsel-pop";
    pop.id = inst.id + "-list";
    pop.setAttribute("role", "listbox");
    pop.hidden = true;
    /* Parked on <body> from the start rather than created on open: it has to
       live outside the panels to escape their overflow, and it has to exist at
       all times for the button's aria-controls to resolve. */
    document.body.appendChild(pop);

    inst.wrap = wrap;
    inst.btn = btn;
    inst.valueEl = value;
    inst.pop = pop;

    nameFromLabel(inst);

    /* The real control stays tabbable and announced unless taken out of both;
       focus is bounced onto the button, so nothing is lost by hiding it. */
    sel.setAttribute("aria-hidden", "true");
    sel.tabIndex = -1;

    bindInstance(inst);
    watch(inst);
    patchSelect(inst);

    sel.xsel = inst;
    instances.push(inst);
    render(inst);
    return inst;
  }

  /* Hiding the select from the accessibility tree severs its <label for>, so
     the label is borrowed by id instead. Clicking it still lands on the
     select, which the focus handler forwards to the button. */
  function nameFromLabel(inst) {
    var sel = inst.select;
    var lab = sel.id
      ? document.querySelector('label[for="' + cssEscape(sel.id) + '"]')
      : null;
    if (lab) {
      if (!lab.id) lab.id = inst.id + "-label";
      inst.btn.setAttribute("aria-labelledby", lab.id);
      return;
    }
    var aria = sel.getAttribute("aria-label");
    if (aria) inst.btn.setAttribute("aria-label", aria);
  }

  function cssEscape(s) {
    return window.CSS && window.CSS.escape
      ? window.CSS.escape(s)
      : s.replace(/["\\]/g, "\\$&");
  }

  function render(inst) {
    var pop = inst.pop;
    pop.textContent = "";
    inst.rows = [];
    inst.opts = [];

    var kids = inst.select.children;
    for (var i = 0; i < kids.length; i++) {
      var kid = kids[i];
      if (kid.tagName === "OPTGROUP") {
        pop.appendChild(buildGroup(inst, kid));
      } else if (kid.tagName === "OPTION") {
        pop.appendChild(buildRow(inst, kid));
      }
      /* Anything else is Alpine's furniture: the <template x-for> the options
         are rendered from stays a child of the select. */
    }

    if (!inst.rows.length) {
      var empty = document.createElement("div");
      empty.className = "xsel-empty";
      empty.textContent = "No options";
      pop.appendChild(empty);
    }

    patchOptions(inst);
    sync(inst);

    /* The rows the cursor pointed at are gone; every index into them is
       stale. Re-seat it on whatever is selected now. */
    inst.active = -1;
    if (inst.open) {
      var at = selectedRow(inst);
      setActive(inst, at >= 0 ? at : nextEnabled(inst, -1, 1), true);
      place(inst);
    }
  }

  function buildGroup(inst, group) {
    var box = document.createElement("div");
    box.className = "xsel-group";
    box.setAttribute("role", "group");
    box.setAttribute("aria-label", group.label || "");
    var head = document.createElement("div");
    head.className = "xsel-group-label";
    head.textContent = group.label || "";
    head.setAttribute("aria-hidden", "true");
    box.appendChild(head);
    var kids = group.children;
    for (var i = 0; i < kids.length; i++) {
      if (kids[i].tagName === "OPTION") box.appendChild(buildRow(inst, kids[i]));
    }
    return box;
  }

  function buildRow(inst, opt) {
    var row = document.createElement("div");
    row.className = "xsel-opt";
    row.id = inst.id + "-o" + inst.rows.length;
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", "false");
    if (opt.disabled || (opt.parentNode && opt.parentNode.disabled)) {
      row.setAttribute("aria-disabled", "true");
    }
    var label = document.createElement("span");
    label.className = "xsel-label";
    label.textContent = opt.label || opt.text || "";
    var tick = document.createElement("span");
    tick.className = "xsel-tick";
    tick.setAttribute("aria-hidden", "true");
    row.appendChild(label);
    row.appendChild(tick);
    inst.rows.push(row);
    inst.opts.push(opt);
    return row;
  }

  /* Mirrors the select's state onto the button and the list; cheap enough to
     call on every open and every focus as a backstop. */
  function sync(inst) {
    var sel = inst.select;
    var chosen = sel.selectedIndex >= 0 ? sel.options[sel.selectedIndex] : null;
    inst.valueEl.textContent = chosen ? chosen.label || chosen.text || "" : "";

    for (var i = 0; i < inst.rows.length; i++) {
      inst.rows[i].setAttribute(
        "aria-selected",
        inst.opts[i] === chosen ? "true" : "false",
      );
    }

    var off = sel.disabled;
    inst.btn.disabled = off;
    if (off && inst.open) close(inst, false);

    if (sel.getAttribute("aria-invalid") === "true") {
      inst.btn.setAttribute("aria-invalid", "true");
    } else {
      inst.btn.removeAttribute("aria-invalid");
    }
  }

  /* Alpine writes the model back to the DOM by setting option.selected, not
     select.value, and a property write fires no event and mutates no attribute
     - a MutationObserver cannot see it. Shadowing the accessor on the instance
     is the only hook that catches every path: x-model, x-bind:selected, and a
     plain `el.value = x` from app.js. Teardown deletes the own property, which
     restores the prototype behaviour. */
  function forward(target, prop, desc, inst) {
    Object.defineProperty(target, prop, {
      configurable: true,
      enumerable: false,
      get: function () {
        return desc.get.call(this);
      },
      set: function (v) {
        desc.set.call(this, v);
        schedule(inst);
      },
    });
  }

  function patchSelect(inst) {
    if (SELECT_VALUE) forward(inst.select, "value", SELECT_VALUE, inst);
    if (SELECT_INDEX) {
      forward(inst.select, "selectedIndex", SELECT_INDEX, inst);
    }
  }

  function patchOptions(inst) {
    if (!OPTION_SELECTED) return;
    for (var i = 0; i < inst.opts.length; i++) {
      var opt = inst.opts[i];
      if (opt.xselPatched) continue;
      opt.xselPatched = true;
      inst.patched.push(opt);
      forward(opt, "selected", OPTION_SELECTED, inst);
    }
  }

  function unpatch(inst) {
    var sel = inst.select;
    delete sel.value;
    delete sel.selectedIndex;
    for (var i = 0; i < inst.patched.length; i++) {
      delete inst.patched[i].selected;
      delete inst.patched[i].xselPatched;
    }
    inst.patched = [];
  }

  function watch(inst) {
    inst.obs = new MutationObserver(function (records) {
      for (var i = 0; i < records.length; i++) {
        var t = records[i].type;
        /* x-for adds and removes options; x-text rewrites their text as a
           character-data change. Either means the list has to be rebuilt. */
        if (t === "childList" || t === "characterData") {
          inst.needRender = true;
          break;
        }
      }
      schedule(inst);
    });
    inst.obs.observe(inst.select, {
      childList: true,
      subtree: true,
      characterData: true,
      attributes: true,
      attributeFilter: ["selected", "disabled", "value", "label", "aria-invalid"],
    });
  }

  /* Coalesced to one frame: Alpine sets option.selected once per option, so a
     six-option select would otherwise redraw six times for one model change. */
  function schedule(inst) {
    if (inst.queued) return;
    inst.queued = true;
    window.requestAnimationFrame(function () {
      inst.queued = false;
      if (!inst.alive) return;
      if (inst.needRender) {
        inst.needRender = false;
        render(inst);
      } else {
        sync(inst);
      }
    });
  }

  function show(inst) {
    if (inst.open || inst.btn.disabled) return;
    if (openInst && openInst !== inst) close(openInst, false);

    /* Anything the frame-coalesced sync has not caught up with yet lands here,
       before the list is ever seen. */
    if (inst.needRender) {
      inst.needRender = false;
      render(inst);
    } else {
      sync(inst);
    }

    inst.open = true;
    inst.btn.setAttribute("aria-expanded", "true");
    inst.pop.hidden = false;
    openInst = inst;

    var start = selectedRow(inst);
    setActive(inst, start >= 0 ? start : nextEnabled(inst, -1, 1), true);
    place(inst);
    bindGlobals();
    window.requestAnimationFrame(function () {
      if (inst.open) inst.pop.classList.add("is-open");
    });
  }

  function close(inst, refocus) {
    if (!inst.open) return;
    inst.open = false;
    inst.typed = "";
    inst.btn.setAttribute("aria-expanded", "false");
    inst.btn.removeAttribute("aria-activedescendant");
    inst.pop.classList.remove("is-open", "is-above");
    inst.pop.hidden = true;
    if (openInst === inst) {
      openInst = null;
      unbindGlobals();
    }
    if (refocus) inst.btn.focus();
  }

  /* Fixed to the viewport and parented to <body>, so the panels and scroll
     boxes the control sits in cannot clip it. Recomputed on scroll and resize
     rather than closed, which is less startling mid-scroll. */
  function place(inst) {
    var pop = inst.pop;
    var r = inst.btn.getBoundingClientRect();
    var vw = document.documentElement.clientWidth;
    var vh = window.innerHeight;

    pop.style.minWidth = r.width + "px";
    pop.style.maxHeight = POP_MAX_H + "px";

    var below = vh - r.bottom - EDGE - POP_GAP;
    var above = r.top - EDGE - POP_GAP;
    var wanted = pop.scrollHeight + 2;
    var flip = wanted > below && above > below;
    var room = flip ? above : below;

    pop.style.maxHeight = Math.max(96, Math.min(POP_MAX_H, room)) + "px";
    pop.classList.toggle("is-above", flip);

    var left = Math.min(r.left, vw - pop.offsetWidth - EDGE);
    if (left < EDGE) left = EDGE;
    var top = flip ? r.top - pop.offsetHeight - POP_GAP : r.bottom + POP_GAP;

    pop.style.left = Math.round(left) + "px";
    pop.style.top = Math.round(top) + "px";
  }

  function selectedRow(inst) {
    for (var i = 0; i < inst.rows.length; i++) {
      if (inst.rows[i].getAttribute("aria-selected") === "true") return i;
    }
    return -1;
  }

  function disabledRow(inst, i) {
    return inst.rows[i].getAttribute("aria-disabled") === "true";
  }

  function nextEnabled(inst, from, step) {
    var n = inst.rows.length;
    for (var i = from + step; i >= 0 && i < n; i += step) {
      if (!disabledRow(inst, i)) return i;
    }
    return -1;
  }

  function edgeEnabled(inst, last) {
    return last
      ? nextEnabled(inst, inst.rows.length, -1)
      : nextEnabled(inst, -1, 1);
  }

  function setActive(inst, i, reveal) {
    if (i < 0 || i >= inst.rows.length) return;
    if (inst.active >= 0 && inst.rows[inst.active]) {
      inst.rows[inst.active].classList.remove("is-active");
    }
    inst.active = i;
    inst.rows[i].classList.add("is-active");
    inst.btn.setAttribute("aria-activedescendant", inst.rows[i].id);
    if (reveal) revealActive(inst);
  }

  function revealActive(inst) {
    var el = inst.rows[inst.active];
    if (!el) return;
    var pop = inst.pop;
    var top = el.offsetTop;
    var bottom = top + el.offsetHeight;
    if (top < pop.scrollTop) pop.scrollTop = top;
    else if (bottom > pop.scrollTop + pop.clientHeight) {
      pop.scrollTop = bottom - pop.clientHeight;
    }
  }

  function pick(inst, i) {
    var opt = inst.opts[i];
    if (!opt || disabledRow(inst, i)) return;
    var sel = inst.select;
    /* By index, not by value: two options are allowed to share a value and
       assigning one would silently land on the first of them. */
    if (sel.selectedIndex !== opt.index) {
      sel.selectedIndex = opt.index;
      /* x-model binds on `change`; `input` goes out too for anything holding a
         plain listener. Both bubble, which Alpine's delegation needs. */
      sel.dispatchEvent(new Event("input", { bubbles: true }));
      sel.dispatchEvent(new Event("change", { bubbles: true }));
    }
    close(inst, true);
  }

  function typeAhead(inst, ch) {
    var now = Date.now();
    if (now - inst.typedAt > TYPEAHEAD_MS) inst.typed = "";
    inst.typedAt = now;
    inst.typed += ch.toLowerCase();

    /* Hammering one key walks through the options starting with it; a real
       word matches as a prefix. Same split every native listbox makes. */
    var q = inst.typed;
    var repeat = true;
    for (var k = 1; k < q.length; k++) {
      if (q.charAt(k) !== q.charAt(0)) {
        repeat = false;
        break;
      }
    }
    var needle = repeat ? q.charAt(0) : q;
    var from = repeat ? inst.active + 1 : Math.max(inst.active, 0);
    var n = inst.rows.length;

    for (var i = 0; i < n; i++) {
      var at = (from + i + n) % n;
      if (disabledRow(inst, at)) continue;
      var text = (inst.opts[at].label || inst.opts[at].text || "").toLowerCase();
      if (text.indexOf(needle) === 0) {
        setActive(inst, at, true);
        return;
      }
    }
  }

  function bindInstance(inst) {
    var sel = inst.select;

    inst.onSelectFocus = function () {
      /* <label for> and any select.focus() land on the hidden control; put the
         caret where the user can see it. */
      if (inst.alive) inst.btn.focus();
    };
    sel.addEventListener("focus", inst.onSelectFocus);

    inst.onSelectChange = function () {
      schedule(inst);
    };
    sel.addEventListener("change", inst.onSelectChange);

    /* A form reset restores the default selection after this event returns and
       does it below JavaScript - no accessor, no attribute - so it is the one
       write neither hook sees. The frame-deferred sync lands after it. */
    if (sel.form) {
      inst.form = sel.form;
      inst.onFormReset = function () {
        schedule(inst);
      };
      inst.form.addEventListener("reset", inst.onFormReset);
    }

    inst.onBtnFocus = function () {
      sync(inst);
    };
    inst.btn.addEventListener("focus", inst.onBtnFocus);

    inst.onBtnClick = function () {
      if (inst.open) close(inst, true);
      else show(inst);
    };
    inst.btn.addEventListener("click", inst.onBtnClick);

    inst.onKey = function (e) {
      keydown(inst, e);
    };
    inst.btn.addEventListener("keydown", inst.onKey);

    /* Keep focus on the button while the pointer works the list - the whole
       pattern rests on aria-activedescendant, not on moving focus. */
    inst.onPopDown = function (e) {
      e.preventDefault();
    };
    inst.pop.addEventListener("mousedown", inst.onPopDown);

    inst.onPopClick = function (e) {
      var row = e.target.closest ? e.target.closest(".xsel-opt") : null;
      if (!row) return;
      var i = inst.rows.indexOf(row);
      if (i >= 0) pick(inst, i);
    };
    inst.pop.addEventListener("click", inst.onPopClick);

    inst.onPopMove = function (e) {
      var row = e.target.closest ? e.target.closest(".xsel-opt") : null;
      if (!row) return;
      var i = inst.rows.indexOf(row);
      if (i >= 0 && i !== inst.active && !disabledRow(inst, i)) {
        setActive(inst, i, false);
      }
    };
    inst.pop.addEventListener("mousemove", inst.onPopMove);
  }

  function keydown(inst, e) {
    var key = e.key;

    if (!inst.open) {
      if (
        key === "ArrowDown" ||
        key === "ArrowUp" ||
        key === "Enter" ||
        key === " " ||
        key === "Spacebar" ||
        key === "Home" ||
        key === "End"
      ) {
        e.preventDefault();
        show(inst);
        if (key === "Home") setActive(inst, edgeEnabled(inst, false), true);
        else if (key === "End") setActive(inst, edgeEnabled(inst, true), true);
        return;
      }
      if (printable(e)) {
        e.preventDefault();
        show(inst);
        typeAhead(inst, key);
      }
      return;
    }

    switch (key) {
      case "ArrowDown":
        e.preventDefault();
        step(inst, 1);
        return;
      case "ArrowUp":
        e.preventDefault();
        step(inst, -1);
        return;
      case "Home":
        e.preventDefault();
        setActive(inst, edgeEnabled(inst, false), true);
        return;
      case "End":
        e.preventDefault();
        setActive(inst, edgeEnabled(inst, true), true);
        return;
      case "PageDown":
        e.preventDefault();
        page(inst, 1);
        return;
      case "PageUp":
        e.preventDefault();
        page(inst, -1);
        return;
      case "Escape":
        e.preventDefault();
        close(inst, true);
        return;
      case "Enter":
        e.preventDefault();
        pick(inst, inst.active);
        return;
      case "Tab":
        /* Not prevented: the list shuts and focus carries on to the next
           control, which is what a keyboard user expects from Tab. */
        close(inst, false);
        return;
      case " ":
      case "Spacebar":
        /* A space part-way through a typed word belongs to the word. */
        if (inst.typed && Date.now() - inst.typedAt <= TYPEAHEAD_MS) break;
        e.preventDefault();
        pick(inst, inst.active);
        return;
    }

    if (printable(e)) {
      e.preventDefault();
      typeAhead(inst, key);
    }
  }

  function printable(e) {
    return (
      e.key && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey
    );
  }

  function step(inst, dir) {
    var to = nextEnabled(inst, inst.active, dir);
    if (to >= 0) setActive(inst, to, true);
  }

  function page(inst, dir) {
    var rows = Math.max(1, Math.floor(inst.pop.clientHeight / 28) - 1);
    var at = inst.active;
    for (var i = 0; i < rows; i++) {
      var to = nextEnabled(inst, at, dir);
      if (to < 0) break;
      at = to;
    }
    setActive(inst, at, true);
  }

  /* Bound only while a list is up, so a page with nothing open costs the
     scroll handler nothing. */
  function bindGlobals() {
    if (globalsBound) return;
    globalsBound = true;
    document.addEventListener("pointerdown", closeIfOutside, true);
    document.addEventListener("focusin", closeIfOutside, true);
    window.addEventListener("scroll", onViewportChange, true);
    window.addEventListener("resize", onViewportChange);
  }

  function unbindGlobals() {
    if (!globalsBound) return;
    globalsBound = false;
    document.removeEventListener("pointerdown", closeIfOutside, true);
    document.removeEventListener("focusin", closeIfOutside, true);
    window.removeEventListener("scroll", onViewportChange, true);
    window.removeEventListener("resize", onViewportChange);
  }

  function closeIfOutside(e) {
    if (!openInst) return;
    if (openInst.pop.contains(e.target) || openInst.wrap.contains(e.target)) {
      return;
    }
    close(openInst, false);
  }

  function onViewportChange() {
    if (openInst) place(openInst);
  }

  function teardown(inst) {
    if (!inst.alive) return;
    inst.alive = false;
    close(inst, false);
    if (inst.obs) inst.obs.disconnect();

    var sel = inst.select;
    sel.removeEventListener("focus", inst.onSelectFocus);
    sel.removeEventListener("change", inst.onSelectChange);
    if (inst.form) inst.form.removeEventListener("reset", inst.onFormReset);
    inst.btn.removeEventListener("focus", inst.onBtnFocus);
    inst.btn.removeEventListener("click", inst.onBtnClick);
    inst.btn.removeEventListener("keydown", inst.onKey);
    inst.pop.removeEventListener("mousedown", inst.onPopDown);
    inst.pop.removeEventListener("click", inst.onPopClick);
    inst.pop.removeEventListener("mousemove", inst.onPopMove);
    if (inst.pop.parentNode) inst.pop.parentNode.removeChild(inst.pop);
    unpatch(inst);

    sel.removeAttribute("aria-hidden");
    sel.removeAttribute("tabindex");
    delete sel.xsel;

    /* The wrapper is the only thing this file ever added to the app's markup. */
    if (inst.wrap.parentNode) {
      inst.wrap.parentNode.insertBefore(sel, inst.wrap);
      inst.wrap.parentNode.removeChild(inst.wrap);
    }

    var at = instances.indexOf(inst);
    if (at >= 0) instances.splice(at, 1);
  }

  function scan() {
    /* x-if drops whole forms; their popups live on <body> and would outlive
       them. */
    for (var i = instances.length - 1; i >= 0; i--) {
      if (!instances[i].wrap.isConnected) teardown(instances[i]);
    }
    if (!fineMq || !fineMq.matches) return;
    var found = document.querySelectorAll("select");
    for (var j = 0; j < found.length; j++) {
      if (eligible(found[j])) enhance(found[j]);
    }
  }

  function scheduleScan() {
    if (scanQueued) return;
    scanQueued = true;
    window.requestAnimationFrame(function () {
      scanQueued = false;
      scan();
    });
  }

  /* Selects arrive late, and the job list redraws constantly, so only
     mutations that carry a select are worth a rescan - the rest is SSE churn. */
  function onDocMutations(records) {
    for (var i = 0; i < records.length; i++) {
      if (carriesSelect(records[i].addedNodes)) return scheduleScan();
      if (carriesSelect(records[i].removedNodes)) return scheduleScan();
    }
  }

  function carriesSelect(nodes) {
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      if (n.nodeType !== 1) continue;
      if (n.tagName === "SELECT" || n.querySelector("select")) return true;
    }
    return false;
  }

  function applyPointerMode() {
    if (fineMq.matches) {
      scan();
    } else {
      for (var i = instances.length - 1; i >= 0; i--) teardown(instances[i]);
    }
  }

  function enhanceSelects() {
    if (!started) {
      started = true;
      fineMq = window.matchMedia("(pointer: fine)");
      /* A tablet docked to a keyboard and mouse flips this mid-session, so the
         query is listened to rather than read once. addListener is the
         fallback for Safari before 14. */
      var onPointerChange = function () {
        applyPointerMode();
      };
      if (fineMq.addEventListener) fineMq.addEventListener("change", onPointerChange);
      else if (fineMq.addListener) fineMq.addListener(onPointerChange);

      docObs = new MutationObserver(onDocMutations);
      docObs.observe(document.documentElement, {
        childList: true,
        subtree: true,
      });
    }
    applyPointerMode();
  }

  window.enhanceSelects = enhanceSelects;

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", enhanceSelects);
  } else {
    enhanceSelects();
  }
})();
