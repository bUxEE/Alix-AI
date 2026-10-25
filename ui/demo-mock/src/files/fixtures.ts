import { deflateSync } from "node:zlib";

// The files the mock starts with, generated here so the repository carries no binary and no third-party asset: two raster images
// (a floor plan and a chart, drawn into an RGBA buffer and encoded as PNG), three short PDFs whose pages match the citations'
// page numbers, and a CSV export. Everything is fictional (Northwind Facilities).

// ---- PNG ----

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n += 1) {
    let c = n;
    for (let k = 0; k < 8; k += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

const crc32 = (bytes: Uint8Array): number => {
  let c = 0xffffffff;
  for (const byte of bytes) c = (CRC_TABLE[(c ^ byte) & 0xff] as number) ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
};

const chunk = (type: string, data: Uint8Array): Buffer => {
  const head = Buffer.alloc(8);
  head.writeUInt32BE(data.length, 0);
  head.write(type, 4, "ascii");
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(Buffer.concat([head.subarray(4), data])), 0);
  return Buffer.concat([head, data, crc]);
};

type Rgb = readonly [number, number, number];

interface Canvas {
  readonly width: number;
  readonly height: number;
  fill: (x: number, y: number, w: number, h: number, color: Rgb) => void;
  frame: (x: number, y: number, w: number, h: number, thickness: number, color: Rgb) => void;
  disc: (cx: number, cy: number, r: number, color: Rgb) => void;
  png: () => Buffer;
}

const createCanvas = (width: number, height: number, background: Rgb): Canvas => {
  const pixels = Buffer.alloc(width * height * 3);
  const set = (x: number, y: number, color: Rgb): void => {
    if (x < 0 || y < 0 || x >= width || y >= height) return;
    const at = (y * width + x) * 3;
    pixels[at] = color[0];
    pixels[at + 1] = color[1];
    pixels[at + 2] = color[2];
  };
  const fill = (x: number, y: number, w: number, h: number, color: Rgb): void => {
    for (let yy = Math.max(0, y); yy < Math.min(height, y + h); yy += 1) for (let xx = Math.max(0, x); xx < Math.min(width, x + w); xx += 1) set(xx, yy, color);
  };
  fill(0, 0, width, height, background);
  return {
    width,
    height,
    fill,
    frame: (x, y, w, h, t, color) => {
      fill(x, y, w, t, color);
      fill(x, y + h - t, w, t, color);
      fill(x, y, t, h, color);
      fill(x + w - t, y, t, h, color);
    },
    disc: (cx, cy, r, color) => {
      for (let yy = -r; yy <= r; yy += 1) for (let xx = -r; xx <= r; xx += 1) if (xx * xx + yy * yy <= r * r) set(cx + xx, cy + yy, color);
    },
    png: () => {
      const raw = Buffer.alloc((width * 3 + 1) * height);
      for (let y = 0; y < height; y += 1) {
        raw[y * (width * 3 + 1)] = 0; // filter: none
        pixels.copy(raw, y * (width * 3 + 1) + 1, y * width * 3, (y + 1) * width * 3);
      }
      const ihdr = Buffer.alloc(13);
      ihdr.writeUInt32BE(width, 0);
      ihdr.writeUInt32BE(height, 4);
      ihdr[8] = 8; // bit depth
      ihdr[9] = 2; // colour type: RGB
      return Buffer.concat([
        Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
        chunk("IHDR", ihdr),
        chunk("IDAT", deflateSync(raw, { level: 9 })),
        chunk("IEND", new Uint8Array(0)),
      ]);
    },
  };
};

/** Level 3 of Harbor Point Office Tower: offices, two meeting rooms, the core, and the HVAC plant room in amber with a technician marker. */
export const floorPlanPng = (): Buffer => {
  const c = createCanvas(720, 440, [247, 245, 240]);
  const wall: Rgb = [58, 64, 74];
  c.fill(40, 40, 640, 360, [255, 255, 255]);
  // rooms
  c.fill(44, 44, 200, 160, [221, 235, 247]); // open office west
  c.fill(44, 236, 200, 160, [221, 235, 247]);
  c.fill(276, 44, 168, 110, [226, 240, 217]); // meeting rooms
  c.fill(276, 286, 168, 110, [226, 240, 217]);
  c.fill(296, 170, 128, 100, [228, 228, 232]); // core: lifts and stairs
  c.fill(476, 44, 200, 200, [221, 235, 247]); // open office east
  c.fill(476, 276, 200, 120, [255, 224, 160]); // HVAC plant room
  // walls
  c.frame(40, 40, 640, 360, 6, wall);
  c.fill(244, 40, 6, 130, wall);
  c.fill(244, 230, 6, 170, wall);
  c.fill(470, 40, 6, 150, wall);
  c.fill(470, 230, 6, 170, wall);
  c.fill(276, 154, 168, 4, wall);
  c.fill(276, 282, 168, 4, wall);
  c.frame(296, 170, 128, 100, 4, wall);
  c.fill(476, 270, 204, 6, wall);
  c.fill(40, 216, 204, 4, wall);
  // doors (gaps)
  c.fill(470, 330, 6, 34, [255, 255, 255]);
  c.fill(344, 154, 32, 4, [255, 255, 255]);
  // equipment in the plant room: two air handling units and the filter bank
  c.fill(500, 300, 64, 40, [214, 120, 40]);
  c.fill(584, 300, 64, 40, [214, 120, 40]);
  c.fill(500, 352, 148, 18, [160, 82, 20]);
  c.disc(612, 380, 9, [200, 30, 60]);
  return c.png();
};

/** Monthly energy use (MWh) for two sites, April to September 2026, as grouped bars. */
export const energyChartPng = (): Buffer => {
  const c = createCanvas(720, 400, [255, 255, 255]);
  const axis: Rgb = [90, 96, 108];
  const grid: Rgb = [232, 234, 238];
  const harbor = [412, 398, 455, 503, 496, 430];
  const westfield = [688, 702, 741, 797, 802, 735];
  const left = 70;
  const bottom = 350;
  const top = 40;
  const scale = (bottom - top) / 900;
  for (let v = 0; v <= 900; v += 150) c.fill(left, Math.round(bottom - v * scale), 620, 1, grid);
  c.fill(left, top, 2, bottom - top, axis);
  c.fill(left, bottom, 620, 2, axis);
  harbor.forEach((value, month) => {
    const x = left + 24 + month * 100;
    c.fill(x, Math.round(bottom - value * scale), 34, Math.round(value * scale), [52, 120, 198]);
    const w = westfield[month] as number;
    c.fill(x + 38, Math.round(bottom - w * scale), 34, Math.round(w * scale), [236, 152, 52]);
  });
  // legend
  c.fill(500, 14, 16, 12, [52, 120, 198]);
  c.fill(600, 14, 16, 12, [236, 152, 52]);
  return c.png();
};

// ---- PDF ----

const pdfString = (text: string): string => `(${text.replace(/\\/g, "\\\\").replace(/\(/g, "\\(").replace(/\)/g, "\\)")})`;

/** A plain PDF 1.4: one page per entry, a title line and body lines in Helvetica. ASCII text only. */
export const simplePdf = (title: string, pages: readonly (readonly string[])[]): Buffer => {
  const objects: string[] = [];
  const pageIds: number[] = [];
  // 1 catalog, 2 pages, 3 font; then a page object and a content object per page
  pages.forEach((lines, index) => {
    const body = [
      "BT",
      "/F1 16 Tf",
      "72 760 Td",
      `${pdfString(title)} Tj`,
      "/F1 11 Tf",
      "0 -28 Td",
      ...lines.flatMap((line) => [`${pdfString(line)} Tj`, "0 -16 Td"]),
      "0 -24 Td",
      `${pdfString(`Northwind Facilities - page ${index + 1} of ${pages.length}`)} Tj`,
      "ET",
    ].join("\n");
    const pageId = 4 + index * 2;
    pageIds.push(pageId);
    objects[pageId] = `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents ${pageId + 1} 0 R >>`;
    objects[pageId + 1] = `<< /Length ${Buffer.byteLength(body, "latin1")} >>\nstream\n${body}\nendstream`;
  });
  objects[1] = "<< /Type /Catalog /Pages 2 0 R >>";
  objects[2] = `<< /Type /Pages /Kids [${pageIds.map((id) => `${id} 0 R`).join(" ")}] /Count ${pageIds.length} >>`;
  objects[3] = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>";
  let out = "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n";
  const offsets: number[] = [];
  for (let id = 1; id < objects.length; id += 1) {
    offsets[id] = Buffer.byteLength(out, "latin1");
    out += `${id} 0 obj\n${objects[id] as string}\nendobj\n`;
  }
  const xref = Buffer.byteLength(out, "latin1");
  out += `xref\n0 ${objects.length}\n0000000000 65535 f \n`;
  for (let id = 1; id < objects.length; id += 1) out += `${String(offsets[id]).padStart(10, "0")} 00000 n \n`;
  out += `trailer\n<< /Size ${objects.length} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(out, "latin1");
};

const filler = (topic: string): string[] => [
  `This page belongs to the ${topic}.`,
  "It is a fictional document generated for the AlixAI demo.",
];

export const maintenancePolicyPdf = (): Buffer =>
  simplePdf("Preventive Maintenance Policy - version 12", [
    ["1. Purpose and scope", "Applies to every site Northwind Facilities manages.", ...filler("maintenance policy")],
    ["2. Responsibilities", "Site managers own the maintenance plan of their site.", ...filler("maintenance policy")],
    ["3. Planned maintenance", "3.1 Inspections follow the annual schedule.", ...filler("maintenance policy")],
    [
      "3.2 Air handling units and filters",
      "Filters of air handling units are replaced every 3 months,",
      "or earlier when the pressure drop exceeds 250 Pa.",
      "Filter class: ePM1 55% (F7) on supply air in offices,",
      "ePM1 80% (F9) in clinical areas.",
      "Replacement is logged as a work order with photos.",
    ],
    ["4. Reactive maintenance", "P1 response within 4 hours, P2 within one business day.", ...filler("maintenance policy")],
    ["5. Records", "Work orders are kept for 7 years.", ...filler("maintenance policy")],
  ]);

export const hvacContractPdf = (): Buffer =>
  simplePdf(
    "HVAC Service Contract - Coolwave Ltd",
    Array.from({ length: 12 }, (_, index) =>
      index === 10
        ? [
            "Schedule B - Consumables",
            "Filters are supplied by the contractor and included in the fee",
            "for up to 4 replacements per unit per year.",
            "Additional replacements are billed at cost plus 8%.",
          ]
        : [`Clause ${index + 1}`, ...filler("Coolwave service contract")],
    ),
  );

export const fireSchedulePdf = (): Buffer =>
  simplePdf("Fire Safety Inspection Schedule 2026", [
    ["Q1 and Q2", "All sites inspected; no open findings."],
    ["Q3", "Riverside Logistics Depot: sprinkler valve tagged for follow-up.", "Elm Street Medical Center: passed."],
    ["Q4", "Riverside Logistics Depot: full inspection due by 31 October.", "Inspector: Fiamma Safety Srl."],
  ]);

export const ventilationPdf = (): Buffer =>
  simplePdf("Elm Street Medical Center - Ventilation Requirements", [
    ["1. Scope", "Clinical and waiting areas of the Elm Street Medical Center."],
    ["2. Filtration", "Clinical areas use ePM1 80% (F9) filters on supply air.", "Pressure drop is checked monthly."],
  ]);

export const workOrdersCsv = (): Buffer =>
  Buffer.from(
    [
      "work_order,site,category,priority,opened,due,status,estimated_hours",
      "WO-2291,Harbor Point Office Tower,HVAC,P2,2026-09-29,2026-10-14,open,3.5",
      "WO-2294,Riverside Logistics Depot,Fire safety,P1,2026-10-01,2026-10-03,overdue,6",
      "WO-2297,Elm Street Medical Center,Plumbing,P3,2026-10-02,2026-10-20,open,2",
      "WO-2301,Westfield Data Hall,Electrical,P1,2026-10-04,2026-10-06,overdue,8",
      "WO-2305,Harbor Point Office Tower,Lifts,P2,2026-10-05,2026-10-12,in progress,4",
      "WO-2309,Riverside Logistics Depot,Doors and access,P3,2026-10-06,2026-10-24,open,1.5",
      "WO-2312,Westfield Data Hall,Cooling,P2,2026-10-07,2026-10-15,open,5",
      "",
    ].join("\n"),
    "utf8",
  );
