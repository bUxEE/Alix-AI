// The showcase's sections, in navigation order. The words for each are in text.ts (`nav.<id>.title`, `nav.<id>.hint`).

export const SECTIONS = ["chat", "conversations", "voice", "files", "settings", "blocks"] as const;

export type SectionId = (typeof SECTIONS)[number];

export const isSectionId = (value: string): value is SectionId => (SECTIONS as readonly string[]).includes(value);
