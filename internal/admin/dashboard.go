package admin

import "net/http"

// dashboardHTML is static: no data is embedded; the page asks the operator
// for the admin token and polls /admin/stats every 2 s.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>White Wizard — dashboard</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;margin:1.5rem;color:#222;background:#fafafa}
h1{font-size:1.3rem}
.tiles{display:flex;gap:1rem;flex-wrap:wrap;margin:1rem 0}
.tile{background:#fff;border:1px solid #ddd;border-radius:6px;padding:.6rem 1rem;min-width:9rem}
.tile b{display:block;font-size:1.5rem}
table{border-collapse:collapse;background:#fff;width:100%}
th,td{border:1px solid #ddd;padding:.3rem .6rem;text-align:right}
th:first-child,td:first-child,th:nth-child(2),td:nth-child(2){text-align:left}
#err{color:#b00}
</style>
</head>
<body>
<h1>White Wizard — control plane</h1>
<form id="f"><label>Admin token <input id="tok" type="password" autocomplete="off"></label> <button>Connect</button></form>
<p id="err"></p>
<div class="tiles">
<div class="tile">req/s<b id="rps">-</b></div>
<div class="tile">requests<b id="req">-</b></div>
<div class="tile">in flight<b id="inf">-</b></div>
<div class="tile">streams open<b id="str">-</b></div>
<div class="tile">errors injected<b id="einj">-</b></div>
<div class="tile">rate limited<b id="rl">-</b></div>
</div>
<h2>Errors by kind</h2><pre id="ek">-</pre>
<h2>By model</h2>
<table><thead><tr><th>driver</th><th>model</th><th>requests</th><th>errors</th><th>TTFT p50 ms</th><th>TTFT p99 ms</th><th>E2E p99 ms</th></tr></thead><tbody id="models"></tbody></table>
<script>
(function(){
  var tok = sessionStorage.getItem("wwz-token") || "";
  var prev = null;
  document.getElementById("tok").value = tok;
  document.getElementById("f").onsubmit = function(e){ e.preventDefault(); tok = document.getElementById("tok").value; sessionStorage.setItem("wwz-token", tok); tick(); };
  function set(id, v){ document.getElementById(id).textContent = v; }
  function fmt(n){ return (Math.round(n*10)/10).toString(); }
  function tick(){
    var h = tok ? {"Authorization": "Bearer " + tok} : {};
    fetch("/admin/stats", {headers: h}).then(function(r){
      if (!r.ok) { throw new Error("HTTP " + r.status); }
      return r.json();
    }).then(function(s){
      set("err", "");
      var t = s.totals, now = Date.now();
      if (prev) { set("rps", fmt((t.requests - prev.n) / ((now - prev.t)/1000))); }
      prev = {n: t.requests, t: now};
      set("req", t.requests); set("inf", t.in_flight); set("str", t.streams_open);
      set("einj", t.errors_injected); set("rl", t.rate_limited);
      set("ek", JSON.stringify(t.errors_by_kind || {}, null, 1));
      var body = document.getElementById("models"); body.textContent = "";
      (s.by_model || []).forEach(function(m){
        var tr = document.createElement("tr");
        [m.driver, m.model, m.requests, m.errors || 0, fmt(m.ttft_p50_ms || 0), fmt(m.ttft_p99_ms || 0), fmt(m.e2e_p99_ms || 0)].forEach(function(v){
          var td = document.createElement("td"); td.textContent = v; tr.appendChild(td);
        });
        body.appendChild(tr);
      });
    }).catch(function(e){ set("err", e.message); });
  }
  tick(); setInterval(tick, 2000);
})();
</script>
</body>
</html>
`

func (s *Server) dashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}
