package diag

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/buxee/alix-ai/internal/metrics"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	// OperatorTokenHeader carries the operator credential on /diagnostics.
	OperatorTokenHeader = "X-Alixai-Admin-Token"
	// OperatorDiagnosticsPath is the diagnostics bundle.
	OperatorDiagnosticsPath = "/diagnostics"

	operatorMinTokenLen = 32
	operatorMaxTokenLen = 256
)

// OperatorHandler is the deployment-wide view for the operator, to be served by
// metrics.Serve on a loopback listener and nowhere else:
//
//	GET /metrics       Prometheus text (queue, providers, resources, process)
//	GET /diagnostics   the redacted diagnostics bundle as JSON
//
// Both are refused unless the request is from the machine itself
// (metrics.LoopbackOnly), and /diagnostics also needs the operator credential in
// OperatorTokenHeader, because it carries the configuration and the recent
// errors. The credential is the one the composition root gives the admin
// interface (gateway.NewAdminToken, kept in the owner-only endpoint file); a
// context token a host application minted is just another wrong value. A
// credential shorter than 32 characters is refused here: an operator interface
// with no real secret does not exist.
func OperatorHandler(svc *Service, token string) (http.Handler, error) {
	if svc == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "diag: the operator handler needs a service")
	}
	if len(token) < operatorMinTokenLen || len(token) > operatorMaxTokenLen {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "diag: the operator credential must be %d to %d characters", operatorMinTokenLen, operatorMaxTokenLen)
	}
	want := sha256.Sum256([]byte(token))
	mux := http.NewServeMux()
	mux.Handle("GET "+metrics.MetricsPath, metrics.Handler(svc.Registry()))
	mux.HandleFunc("GET "+OperatorDiagnosticsPath, func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get(OperatorTokenHeader)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		out, err := svc.MarshalBundle(r.Context())
		if err != nil {
			http.Error(w, "the diagnostics bundle could not be built", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Disposition", `attachment; filename="alixai-diagnostics.json"`)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(out)
	})
	return metrics.LoopbackOnly(mux), nil
}
