// jsdom has no canvas: its getContext reports "not implemented" on every call. Here it answers null quietly, as a browser with no
// WebGL and no Canvas 2D would, so the voice orb renders nothing; the orb's own tests replace it with fake contexts.
if (typeof HTMLCanvasElement !== "undefined") {
  HTMLCanvasElement.prototype.getContext = (() => null) as unknown as typeof HTMLCanvasElement.prototype.getContext;
}
