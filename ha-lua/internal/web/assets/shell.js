// Every URL here is relative: under HA ingress the app lives beneath
// /api/hassio_ingress/<token>/ and the shell cannot know that prefix.
(function () {
  "use strict";

  var nav = document.getElementById("tabs");
  var frame = document.getElementById("page");
  var tabs = [];

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
    if (frame.getAttribute("src") !== src) frame.setAttribute("src", src);
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
