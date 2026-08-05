// Paths are relative to /debug/, which is where this page is served.
(function () {
  "use strict";

  var logEl = document.getElementById("log");
  var levelEl = document.getElementById("level");
  var followEl = document.getElementById("follow");
  var statusEl = document.getElementById("status");
  var since = 0;

  function cells(target, pairs) {
    var host = document.getElementById(target);
    host.textContent = "";
    pairs.forEach(function (pair) {
      var cell = document.createElement("dl");
      cell.className = "cell";
      var term = document.createElement("dt");
      term.textContent = pair[0];
      var value = document.createElement("dd");
      value.textContent = pair[1] === undefined || pair[1] === "" ? "—" : String(pair[1]);
      if (pair[2]) value.className = pair[2];
      cell.appendChild(term);
      cell.appendChild(value);
      host.appendChild(cell);
    });
  }

  function bytes(n) {
    if (!n) return "0 B";
    var units = ["B", "KiB", "MiB", "GiB"];
    var i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
  }

  function clock(iso) {
    if (!iso) return "";
    var when = new Date(iso);
    return isNaN(when) ? "" : when.toLocaleString();
  }

  function renderInfo(info) {
    var rt = info.runtime || {};
    cells("runtime", [
      ["version", rt.version],
      ["uptime", rt.uptime],
      ["go", rt.go],
      ["gomaxprocs", rt.gomaxprocs],
      ["goroutines", rt.goroutines],
      ["heap alloc", bytes(rt.heap_alloc)],
      ["heap sys", bytes(rt.heap_sys)],
      ["gc cycles", rt.num_gc],
      ["pprof", rt.pprof_addr],
    ]);

    var link = info.ha;
    if (link) {
      cells("ha", [
        ["connected", link.connected ? "yes" : "no", link.connected ? "ok" : "bad"],
        ["since", clock(link.connected_since)],
        ["reconnects", link.reconnects, link.reconnects ? "warn" : ""],
        ["url", link.url],
        ["subscribed", (link.subscribed || []).join(", ")],
        ["last error", link.last_error, link.last_error ? "bad" : ""],
        ["last error at", clock(link.last_error_at)],
      ]);
    } else {
      cells("ha", [["client", "not wired"]]);
    }

    var store = info.storage || {};
    cells("storage", [
      ["db", store.path],
      ["size", bytes(store.size)],
      ["entities mirrored", store.entities],
      ["write queue", store.write_queue_len + " / " + store.write_queue_cap,
        store.write_queue_len > store.write_queue_cap / 2 ? "warn" : ""],
      ["retention", store.retention_days + " days"],
      ["purge every", store.purge_interval],
    ]);

    var body = document.querySelector("#scripts tbody");
    body.textContent = "";
    (info.scripts || []).forEach(function (script) {
      var row = document.createElement("tr");
      var routes = (script.routes || []).map(function (route) {
        return route.Method + " " + route.Prefix;
      }).join(", ");
      var timers = (script.timers || []).map(function (timer) {
        return timer.type + " " + timer.spec + " → " + clock(timer.next_run);
      }).join("\n");
      var err = script.last_error;

      [
        [script.script_id, "mono"],
        [script.ui_title || "—", ""],
        [routes || "—", "mono wrapcell"],
        [script.state_handlers + " state / " + script.event_handlers + " event", ""],
        [timers || "—", "wrapcell"],
        [script.queue_len + " / " + script.queue_cap + (script.immediate_events ? " (immediate)" : ""), ""],
        [script.dropped_events, script.dropped_events ? "bad" : "muted"],
        [err ? clock(err.time) + " " + err.callback + ": " + err.error : "—", err ? "bad wrapcell" : "muted"],
      ].forEach(function (cell) {
        var td = document.createElement("td");
        td.textContent = cell[0];
        if (cell[1]) td.className = cell[1];
        row.appendChild(td);
      });
      body.appendChild(row);
    });
  }

  function renderLogs(reply) {
    var atBottom = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 8;
    (reply.records || []).forEach(function (rec) {
      var line = document.createElement("div");
      var level = document.createElement("span");
      level.className = "lvl " + levelClass(rec.level);
      level.textContent = rec.level;
      var text = " " + new Date(rec.time).toLocaleTimeString() + " " + rec.msg;
      Object.keys(rec.attrs || {}).forEach(function (key) {
        text += " " + key + "=" + rec.attrs[key];
      });
      line.appendChild(level);
      line.appendChild(document.createTextNode(text));
      logEl.appendChild(line);
    });
    while (logEl.childElementCount > 1000) logEl.removeChild(logEl.firstChild);
    if (followEl.checked && atBottom) logEl.scrollTop = logEl.scrollHeight;
  }

  function levelClass(level) {
    if (level === "ERROR") return "bad";
    if (level === "WARN") return "warn";
    if (level === "DEBUG") return "muted";
    return "";
  }

  function poll() {
    var logURL = "api/logs?since=" + since + "&level=" + encodeURIComponent(levelEl.value);
    Promise.all([
      fetch("api/info").then(function (r) { return r.json(); }),
      fetch(logURL).then(function (r) { return r.json(); }),
    ]).then(function (results) {
      renderInfo(results[0]);
      since = results[1].newest || since;
      renderLogs(results[1]);
      statusEl.textContent = "updated " + new Date().toLocaleTimeString();
    }).catch(function (err) {
      statusEl.textContent = "poll failed: " + err.message;
    });
  }

  levelEl.addEventListener("change", function () {
    // A stricter filter cannot retroactively hide what is already rendered.
    since = 0;
    logEl.textContent = "";
    poll();
  });
  document.getElementById("clear").addEventListener("click", function () {
    logEl.textContent = "";
  });

  var stacksEl = document.getElementById("stacks");
  var dumpStatusEl = document.getElementById("dumpstatus");
  document.getElementById("dump").addEventListener("click", function () {
    dumpStatusEl.textContent = "capturing…";
    fetch("api/goroutines")
      .then(function (resp) {
        if (!resp.ok) throw new Error("HTTP " + resp.status);
        return resp.text();
      })
      .then(function (text) {
        stacksEl.textContent = text;
        stacksEl.hidden = false;
        dumpStatusEl.textContent = "captured " + new Date().toLocaleTimeString();
      })
      .catch(function (err) {
        dumpStatusEl.textContent = "failed: " + err.message;
      });
  });

  poll();
  setInterval(poll, 3000);
})();
