// Package native holds the native service definitions of AlixAI (M5-02, ADR 0012): the systemd unit, the launchd
// plist and the per-platform configuration. It has no code; its tests are the static checks that keep the
// definitions hardened (SEC-080) and consistent with each other and with the release configuration.
package native
