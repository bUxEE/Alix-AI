// Package polly is the external text-to-speech adapter for Amazon Polly
// SynthesizeSpeech. It implements internal/providers/voice.TTS. Disabled by
// default; wired from internal/cli when external mode is on.
//
// # Research record (checked 2026-10-26)
//
//	https://docs.aws.amazon.com/polly/latest/dg/API_SynthesizeSpeech.html
//	https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
//	https://docs.aws.amazon.com/general/latest/gr/sigv4-create-canonical-request.html
//
// Endpoint: POST https://polly.{region}.amazonaws.com/v1/speech (JSON).
// OutputFormat: pcm (signed 16-bit little-endian mono). SampleRate: "16000"
// (pcm allows 8000 or 16000; default 16000). Engine: neural | standard |
// generative | long-form (default neural). Text limit: 6000 characters total,
// of which at most 3000 are billed; this adapter chunks at 3000 characters.
// Auth: AWS Signature Version 4, implemented in-house (no AWS SDK). Credential
// format stored as ONE credential value:
// "ACCESS_KEY_ID:SECRET_ACCESS_KEY" or, for STS temporary credentials,
// "ACCESS_KEY_ID:SECRET_ACCESS_KEY:SESSION_TOKEN" (the third segment becomes
// X-Amz-Security-Token and is signed). Secrets and tokens must not contain
// ':' (split is on colon). Parse failures surface only as "malformed credential".
//
// Provider id: "polly". Metering unit: characters (schemas/providers meterUnit
// has no characters unit as of this check — report only).
package polly
