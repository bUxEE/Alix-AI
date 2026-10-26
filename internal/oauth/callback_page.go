package oauth

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strings"
)

// CallbackPageInput is the safe data for the loopback callback HTML page. It never holds a code,
// state, token or provider error text.
type CallbackPageInput struct {
	FlowID       string
	Status       string // completed | failed | cancelled
	OpenerOrigin string // exact targetOrigin; empty skips postMessage
	Nonce        string // CSP script nonce
	CLIClose     bool   // "AlixAI is connected..." copy for operator browser login
}

var callbackPageTmpl = template.Must(template.New("oauth-callback").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AlixAI</title>
</head>
<body>
<p>{{.Message}}</p>
{{if .Script}}
<script nonce="{{.Nonce}}">{{.Script}}</script>
{{end}}
</body>
</html>
`))

// CallbackCSP is the Content-Security-Policy for the callback page (nonce-based scripts, no external resources).
func CallbackCSP(nonce string) string {
	return "default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'none'; img-src 'none'; connect-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

// RenderCallbackPage returns the HTML body. Status is one of completed, failed, cancelled.
func RenderCallbackPage(in CallbackPageInput) []byte {
	msg := "You can close this window"
	if in.CLIClose {
		msg = "AlixAI is connected, you can close this window"
	}
	data := struct {
		Message string
		Nonce   string
		Script  template.JS
	}{Message: msg, Nonce: in.Nonce}
	if in.OpenerOrigin != "" && in.FlowID != "" {
		payload := map[string]string{
			"type":    "alixai.oauth.result",
			"flow_id": in.FlowID,
			"status":  sanitizePageStatus(in.Status),
		}
		raw, _ := json.Marshal(payload)
		// targetOrigin is a Go string literal inside JS; never use *.
		originLit, _ := json.Marshal(in.OpenerOrigin)
		data.Script = template.JS(`(function(){var m=` + string(raw) + `;var o=` + string(originLit) + `;if(window.opener){try{window.opener.postMessage(m,o);}catch(e){}}window.close();})();`)
	}
	var buf bytes.Buffer
	_ = callbackPageTmpl.Execute(&buf, data)
	return buf.Bytes()
}

func sanitizePageStatus(s string) string {
	switch strings.ToLower(s) {
	case "completed", "failed", "cancelled":
		return strings.ToLower(s)
	default:
		return "failed"
	}
}
