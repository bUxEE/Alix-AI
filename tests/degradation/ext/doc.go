// Package ext holds one test per row of the failure and fallback table of the External Provider Mode additive plan
// (section 13, task X-18). Like the section 18 matrix next to it, each row's "Required behavior" is asserted
// literally against the real components: the dialogue engine, the policy gateway, the action coordinator and the
// session store, the external-mode runtime and its provider router, and the data-egress service. The external
// provider is the scripted fake with failures injected where the table says they happen; the host application is
// the clinic reference stand-in behind a counting adapter, so "no commit" is read off the host, not inferred.
// Nothing here contacts a real provider or uses a real key. The last test is the table's closing sentence: with
// every provider failing, ordinary host-application work still runs.
package ext
