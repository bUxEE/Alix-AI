import type { OrbPalette, Rgb } from "./color.js";
import { cssRgb } from "./color.js";
import { edgeWave } from "./motion.js";
import type { OrbFrame } from "./motion.js";

// The two ways the orb is painted. WebGL 1 with one small fragment shader (no extension) is the default; Canvas 2D radial
// gradients draw the same sphere where WebGL is missing or its context was lost. Both draw from the same frame and palette.

export interface OrbRenderer {
  readonly kind: "webgl" | "2d";
  draw: (frame: OrbFrame, palette: OrbPalette, width: number, height: number) => void;
  dispose: () => void;
}

const VERTEX = `
attribute vec2 aPos;
uniform vec2 uAspect;
varying vec2 vPos;
void main() {
  vPos = aPos * uAspect;
  gl_Position = vec4(aPos, 0.0, 1.0);
}`;

// The sphere: an edge that wobbles with the flow, a lit body (a fake normal from the disc), soft inner currents twisted toward the
// centre by the swirl, a rim of light and a premultiplied halo. Every multiplier of uFlow has one decimal (see PHASE_WRAP).
const FRAGMENT = `
#ifdef GL_FRAGMENT_PRECISION_HIGH
precision highp float;
#else
precision mediump float;
#endif
uniform float uRadius;
uniform float uWobble;
uniform float uFlow;
uniform float uSwirl;
uniform float uHalo;
uniform float uAa;
uniform vec3 uDeep;
uniform vec3 uMid;
uniform vec3 uLight;
uniform vec3 uHaloColor;
varying vec2 vPos;

void main() {
  vec2 p = vPos;
  float len = length(p);
  float a = atan(p.y, p.x);
  float wave = sin(3.0 * a + uFlow * 1.3) * 0.5 + sin(5.0 * a - uFlow * 1.7) * 0.3 + sin(2.0 * a + uFlow * 0.7 + 1.1) * 0.2;
  float r = uRadius * (1.0 + uWobble * wave);
  float d = len - r;
  float body = 1.0 - smoothstep(-uAa, uAa, d);

  vec2 q = p / max(r, 0.001);
  float ql = min(length(q), 1.0);
  float twist = uSwirl * (1.0 - ql) * 2.4 + uFlow * 0.2;
  float c = cos(twist);
  float s = sin(twist);
  vec2 w = vec2(c * q.x - s * q.y, s * q.x + c * q.y);
  float current = sin(w.x * 1.8 + uFlow * 0.9) * sin(w.y * 1.6 - uFlow * 0.7) + 0.6 * sin((w.x - w.y) * 2.3 + uFlow * 1.1);
  current = smoothstep(0.05, 1.25, current);

  // Lit from within more than from outside: a gentle shade, clouds of light drifting inside, a glowing core and a bright rim.
  float z = sqrt(max(1.0 - ql * ql, 0.0));
  vec3 n = vec3(q, z);
  vec3 l = normalize(vec3(-0.45, 0.55, 0.7));
  float diffuse = clamp(dot(n, l), 0.0, 1.0);
  vec3 color = mix(uDeep, uMid, smoothstep(-0.1, 0.75, diffuse));
  color = mix(color, uLight, smoothstep(0.45, 1.0, diffuse) * 0.4);
  color = mix(color, uLight, current * 0.62 * (0.35 + 0.65 * z));
  color = mix(color, uLight, (1.0 - ql) * (1.0 - ql) * 0.16);
  float spec = pow(max(dot(reflect(-l, n), vec3(0.0, 0.0, 1.0)), 0.0), 10.0);
  color = mix(color, uLight, spec * 0.16);
  color = mix(color, uLight, pow(1.0 - z, 2.5) * 0.42);

  float halo = uHalo * exp(-max(d, 0.0) * 7.0) * (1.0 - smoothstep(0.82, 1.0, len)) * (1.0 - body);
  gl_FragColor = vec4(color * body + uHaloColor * halo, body + halo);
}`;

const UNIFORMS = ["uAspect", "uRadius", "uWobble", "uFlow", "uSwirl", "uHalo", "uAa", "uDeep", "uMid", "uLight", "uHaloColor"] as const;
type Uniform = (typeof UNIFORMS)[number];

const compile = (gl: WebGLRenderingContext, type: number, source: string): WebGLShader | undefined => {
  const shader = gl.createShader(type);
  if (shader === null) return undefined;
  gl.shaderSource(shader, source);
  gl.compileShader(shader);
  if (gl.getShaderParameter(shader, gl.COMPILE_STATUS) !== true && !gl.isContextLost()) {
    gl.deleteShader(shader);
    return undefined;
  }
  return shader;
};

/** A WebGL renderer for this context, or undefined when the program cannot be built. */
export const createWebglRenderer = (gl: WebGLRenderingContext): OrbRenderer | undefined => {
  const vertex = compile(gl, gl.VERTEX_SHADER, VERTEX);
  const fragment = compile(gl, gl.FRAGMENT_SHADER, FRAGMENT);
  const program = gl.createProgram();
  if (vertex === undefined || fragment === undefined || program === null) return undefined;
  gl.attachShader(program, vertex);
  gl.attachShader(program, fragment);
  gl.linkProgram(program);
  if (gl.getProgramParameter(program, gl.LINK_STATUS) !== true && !gl.isContextLost()) return undefined;
  const buffer = gl.createBuffer();
  if (buffer === null) return undefined;
  // One triangle that covers the whole viewport.
  gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
  const position = gl.getAttribLocation(program, "aPos");
  const at = {} as Record<Uniform, WebGLUniformLocation | null>;
  for (const name of UNIFORMS) at[name] = gl.getUniformLocation(program, name);
  const color = (name: Uniform, value: Rgb): void => gl.uniform3f(at[name], value[0], value[1], value[2]);

  return {
    kind: "webgl",
    draw: (frame, palette, width, height) => {
      const side = Math.max(1, Math.min(width, height));
      gl.viewport(0, 0, width, height);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.useProgram(program);
      gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
      gl.enableVertexAttribArray(position);
      gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);
      gl.uniform2f(at.uAspect, width / side, height / side);
      gl.uniform1f(at.uRadius, frame.radius);
      gl.uniform1f(at.uWobble, frame.wobble);
      gl.uniform1f(at.uFlow, frame.flow);
      gl.uniform1f(at.uSwirl, frame.swirl);
      gl.uniform1f(at.uHalo, frame.glow * palette.haloAlpha);
      gl.uniform1f(at.uAa, 3 / side);
      color("uDeep", palette.deep);
      color("uMid", palette.mid);
      color("uLight", palette.light);
      color("uHaloColor", palette.halo);
      gl.drawArrays(gl.TRIANGLES, 0, 3);
    },
    dispose: () => {
      if (gl.isContextLost()) return;
      gl.deleteBuffer(buffer);
      gl.deleteProgram(program);
      gl.deleteShader(vertex);
      gl.deleteShader(fragment);
    },
  };
};

const SEGMENTS = 72;

/** The same sphere with Canvas 2D: radial gradients clipped to the wobbling edge, two drifting inner currents and a halo. */
export const createCanvasRenderer = (context: CanvasRenderingContext2D): OrbRenderer => ({
  kind: "2d",
  draw: (frame, palette, width, height) => {
    const unit = Math.max(1, Math.min(width, height)) / 2;
    const cx = width / 2;
    const cy = height / 2;
    const radius = frame.radius * unit;
    context.clearRect(0, 0, width, height);

    const haloAlpha = frame.glow * palette.haloAlpha;
    if (haloAlpha > 0.005) {
      const halo = context.createRadialGradient(cx, cy, radius * 0.92, cx, cy, unit);
      halo.addColorStop(0, cssRgb(palette.halo, haloAlpha));
      halo.addColorStop(0.45, cssRgb(palette.halo, haloAlpha * 0.25));
      halo.addColorStop(1, cssRgb(palette.halo, 0));
      context.fillStyle = halo;
      context.fillRect(0, 0, width, height);
    }

    context.save();
    context.beginPath();
    for (let index = 0; index <= SEGMENTS; index += 1) {
      const angle = (index / SEGMENTS) * Math.PI * 2;
      const r = radius * (1 + frame.wobble * edgeWave(angle, frame.flow));
      const x = cx + Math.cos(angle) * r;
      const y = cy - Math.sin(angle) * r; // the shader's y points up
      if (index === 0) context.moveTo(x, y);
      else context.lineTo(x, y);
    }
    context.closePath();
    context.clip();

    const body = context.createRadialGradient(cx - radius * 0.38, cy - radius * 0.42, radius * 0.04, cx, cy, radius * 1.08);
    body.addColorStop(0, cssRgb(palette.light));
    body.addColorStop(0.5, cssRgb(palette.mid));
    body.addColorStop(1, cssRgb(palette.deep));
    context.fillStyle = body;
    context.fillRect(cx - radius * 1.3, cy - radius * 1.3, radius * 2.6, radius * 2.6);

    for (let index = 0; index < 2; index += 1) {
      const angle = frame.flow * (0.3 + index * 0.2) + frame.swirl * 1.4 + index * Math.PI;
      const x = cx + Math.cos(angle) * radius * 0.38;
      const y = cy + Math.sin(angle) * radius * 0.32;
      const current = context.createRadialGradient(x, y, 0, x, y, radius * 0.7);
      current.addColorStop(0, cssRgb(palette.light, 0.3));
      current.addColorStop(1, cssRgb(palette.light, 0));
      context.fillStyle = current;
      context.fillRect(cx - radius * 1.3, cy - radius * 1.3, radius * 2.6, radius * 2.6);
    }
    context.restore();
  },
  dispose: () => undefined,
});
