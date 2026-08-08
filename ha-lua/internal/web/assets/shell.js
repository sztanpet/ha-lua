// Every URL here is relative: under HA ingress the app lives beneath
// /api/hassio_ingress/<token>/ and the shell cannot know that prefix.
(function () {
  "use strict";

  var nav = document.getElementById("tabs");
  var view = document.getElementById("view");
  var frame = document.getElementById("page");
  var tabs = [];
  var watching = null;

  // The framed page must not be a scroller of its own: in the HA companion
  // app's WebView a nested frame ignored touch-scrolls until it had been tapped
  // once. Growing the iframe to its document's height leaves #view as the only
  // scroller.
  function fit() {
    var doc = frame.contentDocument;
    var body = doc && doc.body;
    if (!body) return; // cross-origin or still about:blank
    frame.style.height = Math.max(body.scrollHeight, view.clientHeight) + "px";
    // A page sized against the viewport (html,body{height:100%}) never grows
    // its body, so the frame would go on scrolling itself. Grow it to whatever
    // it is trying to scroll instead; one pass converges unless it reflows.
    var root = doc.scrollingElement;
    for (var i = 0; i < 3 && root && root.scrollHeight > root.clientHeight + 1; i++) {
      frame.style.height = root.scrollHeight + "px";
    }
  }

  function watch() {
    if (watching) watching.disconnect();
    watching = null;
    fit();
    var body = frame.contentDocument && frame.contentDocument.body;
    if (!body || !window.ResizeObserver) return;
    // Script pages repaint on their own poll interval; the frame follows.
    watching = new ResizeObserver(fit);
    watching.observe(body);
  }

  frame.addEventListener("load", watch);
  window.addEventListener("resize", fit);

  function activeID() {
    var wanted = decodeURIComponent(location.hash.replace(/^#/, ""));
    for (var i = 0; i < tabs.length; i++) {
      if (tabs[i].id === wanted) return wanted;
    }
    return tabs.length ? tabs[0].id : "";
  }

  function render() {
    var active = activeID();

    nav.textContent = "";
    tabs.forEach(function (tab) {
      var link = document.createElement("a");
      link.href = "#" + encodeURIComponent(tab.id);
      link.textContent = tab.title;
      if (tab.id === active) link.className = "active";
      nav.appendChild(link);
    });

    var tab = tabs.find(function (candidate) { return candidate.id === active; });
    // Re-assigning the same src would reload the page and lose its state.
    var src = tab ? tab.path : "about:blank";
    if (frame.getAttribute("src") !== src) {
      frame.setAttribute("src", src);
      view.scrollTop = 0; // #view outlives the page it scrolls
    }
    document.title = tab ? "ha-lua — " + tab.title : "ha-lua";
  }

  fetch("api/tabs")
    .then(function (resp) {
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      return resp.json();
    })
    .then(function (list) {
      tabs = list;
      if (!tabs.length) {
        nav.innerHTML = '<span class="err">No script has a UI. Call ha.ui("Title") in a script to add a tab.</span>';
        return;
      }
      render();
      window.addEventListener("hashchange", render);
    })
    .catch(function (err) {
      nav.innerHTML = '<span class="err"></span>';
      nav.firstChild.textContent = "Could not load the tab list: " + err.message;
    });
})();
