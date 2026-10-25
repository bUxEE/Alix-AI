// A small, safe Markdown reader for assistant text. The core sanitises Markdown before it is emitted, and the kit still treats
// the text as untrusted: it is parsed into a closed tree of plain nodes and rendered as text by the Vue template. There is no
// HTML path at all (no `v-html`, no raw HTML node), and a link survives only with an http, https or mailto address.
//
// Supported: paragraphs, headings (1 to 4), fenced code, block quotes, ordered and unordered lists, horizontal rules, and the
// inline forms code, bold, italic and link. Anything else is shown as the text it is. Text that is still streaming parses too:
// an unclosed fence is code to the end, an unclosed `*` is a star.

export type Inline =
  | { kind: "text"; text: string }
  | { kind: "code"; text: string }
  | { kind: "bold"; text: string }
  | { kind: "italic"; text: string }
  | { kind: "link"; text: string; href: string };

export type MdBlock =
  | { kind: "paragraph"; inline: Inline[] }
  | { kind: "heading"; level: 1 | 2 | 3 | 4; inline: Inline[] }
  | { kind: "code"; language: string; text: string }
  | { kind: "quote"; inline: Inline[] }
  | { kind: "list"; ordered: boolean; items: Inline[][] }
  | { kind: "rule" };

const SAFE_HREF = /^(https?:\/\/|mailto:)/i;

/** True for an address a link may carry. Everything else (javascript:, data:, relative paths) is shown as text. */
export const isSafeHref = (href: string): boolean => SAFE_HREF.test(href.trim()) && !/[\u0000-\u001f\s]/.test(href.trim());

// CommonMark: a backslash before ASCII punctuation makes it a literal character. The core's sanitiser escapes every character
// that could start a construct, so `snake\_case` and `2 \* 3` are ordinary text. Inside a code span a backslash is a backslash.
const ESCAPABLE = /^[!-/:-@[-`{-~]$/;
const unescapeText = (text: string): string => text.replace(/\\([!-/:-@[-`{-~])/g, "$1");

export const parseInline = (source: string, links = true): Inline[] => {
  const out: Inline[] = [];
  let text = "";
  const flush = (): void => {
    if (text !== "") out.push({ kind: "text", text });
    text = "";
  };
  let i = 0;
  while (i < source.length) {
    const rest = source.slice(i);
    let match: RegExpExecArray | null;
    if (source[i] === "\\" && ESCAPABLE.test(source[i + 1] ?? "")) {
      text += source[i + 1];
      i += 2;
    } else if ((match = /^`([^`\n]+)`/.exec(rest)) !== null) {
      flush();
      out.push({ kind: "code", text: match[1] as string });
      i += match[0].length;
    } else if ((match = /^(\*\*|__)(?=\S)((?:\\[^\n]|[^\n\\])+?)(?<=\S)\1/.exec(rest)) !== null) {
      flush();
      out.push({ kind: "bold", text: unescapeText(match[2] as string) });
      i += match[0].length;
    } else if ((match = /^(\*|_)(?=\S)((?:\\[^\n]|[^\n*_\\])+?)(?<=\S)\1/.exec(rest)) !== null && (i === 0 || !/\w/.test(source[i - 1] as string))) {
      flush();
      out.push({ kind: "italic", text: unescapeText(match[2] as string) });
      i += match[0].length;
    } else if ((match = /^\[((?:\\[^\n]|[^\]\\\n])+)\]\((?:<([^<>\n]+)>|([^)\s<][^)\s]*))\)/.exec(rest)) !== null) {
      // The core writes the address of a link it allowed in angle brackets: `[label](<https://…>)`.
      flush();
      const label = unescapeText(match[1] as string);
      const href = (match[2] ?? match[3]) as string;
      if (links && isSafeHref(href)) out.push({ kind: "link", text: label, href });
      else out.push({ kind: "text", text: label });
      i += match[0].length;
    } else if ((match = /^<([A-Za-z][A-Za-z0-9+.-]{1,31}:[^<>\s]*)>/.exec(rest)) !== null) {
      // An autolink, `<https://…>` or `<mailto:…>`: the core writes one only for an allowed address and escapes every other `<`.
      flush();
      const href = match[1] as string;
      if (links && isSafeHref(href)) out.push({ kind: "link", text: href, href });
      else out.push({ kind: "text", text: href });
      i += match[0].length;
    } else {
      text += source[i];
      i += 1;
    }
  }
  flush();
  return out;
};

// Matched against a trimmed line, so it needs no trailing `\s*`: a second whitespace run there made it quadratic.
const FENCE = /^(`{3,}|~{3,})\s*([\w+#.-]*)$/;

// The text of a heading after its marker, without a closing run of `#` and the spaces around it (one character at least).
// Trimmed by hand, not by a pattern: a backtracking one is cubic in untrusted text that is parsed again on every streamed token.
const headingText = (rest: string): string => {
  let end = rest.trimEnd().length;
  while (end > 0 && rest[end - 1] === "#") end -= 1;
  const text = rest.slice(0, end).trimEnd();
  return text === "" ? rest.slice(0, 1) : text;
};

// The lines of a paragraph as one run of text. A line that ends in an unescaped backslash ends in a hard line break (the core
// writes one so), which reads as the space between the lines here. Counted by hand, not by a pattern, for the same reason as above.
const joinLines = (lines: string[]): string =>
  lines
    .map((line, index) => {
      let backslashes = 0;
      while (backslashes < line.length && line[line.length - 1 - backslashes] === "\\") backslashes += 1;
      return index < lines.length - 1 && backslashes % 2 === 1 ? line.slice(0, -1) : line;
    })
    .join(" ");

export const parseMarkdown = (source: string, links = true): MdBlock[] => {
  const lines = source.replace(/\r\n?/g, "\n").split("\n");
  const blocks: MdBlock[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i] as string;
    if (line.trim() === "") {
      i += 1;
      continue;
    }
    const fence = FENCE.exec(line.trim());
    if (fence !== null) {
      const marker = (fence[1] as string)[0] as string;
      const length = (fence[1] as string).length;
      const body: string[] = [];
      i += 1;
      while (i < lines.length) {
        const candidate = (lines[i] as string).trim();
        if (candidate.length >= length && candidate.split("").every((char) => char === marker)) break;
        body.push(lines[i] as string);
        i += 1;
      }
      i += 1;
      while (body.length > 0 && (body[body.length - 1] as string).trim() === "") body.pop();
      blocks.push({ kind: "code", language: (fence[2] as string).toLowerCase(), text: body.join("\n") });
      continue;
    }
    const heading = /^(#{1,4})\s+/.exec(line);
    const title = heading === null ? "" : headingText(line.slice(heading[0].length));
    if (heading !== null && title !== "") {
      blocks.push({ kind: "heading", level: (heading[1] as string).length as 1 | 2 | 3 | 4, inline: parseInline(title, links) });
      i += 1;
      continue;
    }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) {
      blocks.push({ kind: "rule" });
      i += 1;
      continue;
    }
    if (/^>\s?/.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && /^>\s?/.test(lines[i] as string)) {
        quoted.push((lines[i] as string).replace(/^>\s?/, ""));
        i += 1;
      }
      blocks.push({ kind: "quote", inline: parseInline(joinLines(quoted), links) });
      continue;
    }
    const bullet = /^\s*([-*+])\s+(.*)$/.exec(line);
    const numbered = /^\s*\d+[.)]\s+(.*)$/.exec(line);
    if (bullet !== null || numbered !== null) {
      const ordered = numbered !== null;
      const pattern = ordered ? /^\s*\d+[.)]\s+(.*)$/ : /^\s*[-*+]\s+(.*)$/;
      const items: Inline[][] = [];
      while (i < lines.length) {
        const item = pattern.exec(lines[i] as string);
        if (item === null) break;
        items.push(parseInline(item[1] as string, links));
        i += 1;
      }
      blocks.push({ kind: "list", ordered, items });
      continue;
    }
    const paragraph: string[] = [];
    while (i < lines.length && (lines[i] as string).trim() !== "" && !FENCE.test((lines[i] as string).trim()) && !/^(#{1,4}\s|>\s?|\s*[-*+]\s|\s*\d+[.)]\s)/.test(lines[i] as string)) {
      paragraph.push((lines[i] as string).trim());
      i += 1;
    }
    if (paragraph.length === 0) {
      // A line no rule above took (a lone marker): show it as text so the loop always advances.
      paragraph.push(line.trim());
      i += 1;
    }
    blocks.push({ kind: "paragraph", inline: parseInline(joinLines(paragraph), links) });
  }
  return blocks;
};
