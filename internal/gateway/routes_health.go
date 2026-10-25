package gateway

import "net/http"

func init() { RegisterArea("health", registerHealth) }

// registerHealth mounts the two probes. Neither takes a body, a query or an
// identity, and neither reveals more than liveness and role status.
func registerHealth(rt *Router, d *Deps) {
	// /healthz is liveness: the process is serving HTTP. It checks no
	// dependency, so a slow model runtime never gets the service restarted.
	rt.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /readyz is readiness: 200 when the service can answer (the controller
	// role is usable and every check passes), 503 when it cannot. A degraded
	// service (a missing router or embedder) is still ready.
	rt.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		rep := d.ready.Report(r.Context())
		status := http.StatusOK
		if !rep.Ready {
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "1")
		}
		writeJSON(w, status, rep)
	})
}
