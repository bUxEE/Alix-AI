package main

import (
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// endpointURL is the address of an endpoint, for the report and as the default target of a run:
// the url of an mcp endpoint, the base_url of an openapi one, the loopback address (host:port) of a
// native one reached over loopback_tcp. An events endpoint, or a native one reached over a socket or
// a pipe, has none to retarget.
func endpointURL(ep ap.ManifestEndpoint) string {
	switch v := ep.Value.(type) {
	case ap.ManifestMCPEndpoint:
		return string(v.URL)
	case ap.ManifestOpenAPIEndpoint:
		return string(v.BaseURL)
	case ap.ManifestNativeEndpoint:
		if v.IPC.Kind == ap.ManifestNativeIPCKindLoopbackTCP {
			return v.IPC.Address
		}
	}
	return ""
}
