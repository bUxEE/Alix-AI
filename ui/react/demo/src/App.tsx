import alixIcon from "../../../../docs/assets/brand/alix-icon.svg";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import { AlixProvider } from "@alixai/react";
import { DEMO_THREAD_ID } from "@alixai/demo-mock";
import { DEMO_TEXT } from "./lib/text";
import type { DemoLang, SectionId } from "./lib/text";
import { ShellContext } from "./shell/context";
import type { DemoEvent, DemoShell } from "./shell/context";
import { Events } from "./shell/Events";
import { Icon } from "./shell/Icon";
import type { IconName } from "./shell/Icon";
import { Segmented } from "./shell/Segmented";
import { AuditSection } from "./sections/AuditSection";
import { BlocksSection } from "./sections/BlocksSection";
import { ChatSection } from "./sections/ChatSection";
import { ConversationsSection } from "./sections/ConversationsSection";
import { FilesSection } from "./sections/FilesSection";
import { SettingsSection } from "./sections/SettingsSection";
import { SourcesSection } from "./sections/SourcesSection";
import { VoiceSection } from "./sections/VoiceSection";

type Theme = "auto" | "light" | "dark";

const NAV: readonly { group: "conversation" | "panels" | "reference"; items: readonly { id: SectionId; icon: IconName }[] }[] = [
  {
    group: "conversation",
    items: [
      { id: "chat", icon: "chat" },
      { id: "conversations", icon: "conversations" },
      { id: "voice", icon: "voice" },
      { id: "files", icon: "files" },
    ],
  },
  {
    group: "panels",
    items: [
      { id: "settings", icon: "settings" },
      { id: "sources", icon: "sources" },
      { id: "audit", icon: "audit" },
    ],
  },
  { group: "reference", items: [{ id: "blocks", icon: "blocks" }] },
];

const SECTIONS: readonly SectionId[] = NAV.flatMap((group) => group.items.map((item) => item.id));

// Per-viewer conveniences only (language, theme): the page works the same when storage is unavailable.
const remembered = <T extends string>(key: string, allowed: readonly T[], fallback: T): T => {
  try {
    const value = window.localStorage.getItem(`alixai-react-demo:${key}`);
    return value !== null && (allowed as readonly string[]).includes(value) ? (value as T) : fallback;
  } catch {
    return fallback;
  }
};
const remember = (key: string, value: string): void => {
  try {
    window.localStorage.setItem(`alixai-react-demo:${key}`, value);
  } catch {
    // storage is unavailable: nothing to keep
  }
};

const sectionFromHash = (): SectionId => {
  const id = window.location.hash.replace(/^#\/?/, "");
  return (SECTIONS as readonly string[]).includes(id) ? (id as SectionId) : "chat";
};

const SectionView = ({ id }: { id: SectionId }): ReactElement => {
  switch (id) {
    case "chat":
      return <ChatSection />;
    case "conversations":
      return <ConversationsSection />;
    case "voice":
      return <VoiceSection />;
    case "files":
      return <FilesSection />;
    case "settings":
      return <SettingsSection />;
    case "sources":
      return <SourcesSection />;
    case "audit":
      return <AuditSection />;
    case "blocks":
      return <BlocksSection />;
  }
};

export const App = (): ReactElement => {
  const [section, setSection] = useState<SectionId>(sectionFromHash);
  const [lang, setLang] = useState<DemoLang>(() => remembered("lang", ["en", "it"] as const, "en"));
  const [theme, setTheme] = useState<Theme>(() => remembered("theme", ["auto", "light", "dark"] as const, "auto"));
  const [threadId, setThreadId] = useState(DEMO_THREAD_ID);
  const [events, setEvents] = useState<DemoEvent[]>([]);
  const counter = useRef(0);
  const main = useRef<HTMLElement>(null);
  const text = DEMO_TEXT[lang];

  useEffect(() => {
    const onHash = (): void => setSection(sectionFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  useEffect(() => {
    const root = document.documentElement;
    if (theme === "auto") delete root.dataset["alixaiTheme"];
    else root.dataset["alixaiTheme"] = theme;
    remember("theme", theme);
  }, [theme]);

  useEffect(() => {
    document.documentElement.lang = lang;
    remember("lang", lang);
  }, [lang]);

  useEffect(() => {
    document.title = `${text.sections[section].title} · AlixAI ${text.kit}`;
  }, [section, text]);

  const go = useCallback((next: SectionId) => {
    window.location.hash = `/${next}`;
    setSection(next);
    main.current?.focus({ preventScroll: true });
    window.scrollTo({ top: 0 });
  }, []);

  const notify = useCallback((name: string, detail: string) => {
    counter.current += 1;
    const id = counter.current;
    setEvents((current) => [...current.slice(-3), { id, name, detail }]);
    setTimeout(() => setEvents((current) => current.filter((event) => event.id !== id)), 5200);
  }, []);

  const openThread = useCallback(
    (next: string) => {
      setThreadId(next);
      go("chat");
    },
    [go],
  );

  const shell = useMemo<DemoShell>(() => ({ lang, theme, text, threadId, openThread, go, notify }), [lang, theme, text, threadId, openThread, go, notify]);

  return (
    <ShellContext.Provider value={shell}>
      <AlixProvider locale={lang}>
        <div className="demo-app">
          <aside className="demo-side">
            <a className="demo-brand" href="#/chat" onClick={() => go("chat")}>
              <img className="demo-brand__mark" src={alixIcon} alt="" aria-hidden="true" />
              <span className="demo-brand__name">
                {text.product}
                <span className="demo-brand__kit">{text.kit}</span>
              </span>
            </a>
            <nav className="demo-nav" aria-label={text.kit}>
              {NAV.map((group) => (
                <div key={group.group} className="demo-nav__group">
                  <p className="demo-nav__heading">{text.groups[group.group]}</p>
                  <ul className="demo-nav__list">
                    {group.items.map((item) => (
                      <li key={item.id}>
                        <a
                          className="demo-nav__item"
                          href={`#/${item.id}`}
                          aria-current={item.id === section ? "page" : undefined}
                          onClick={(event) => {
                            event.preventDefault();
                            go(item.id);
                          }}
                        >
                          <Icon name={item.icon} />
                          <span>{text.sections[item.id].nav}</span>
                        </a>
                      </li>
                    ))}
                  </ul>
                </div>
              ))}
            </nav>
            <div className="demo-side__foot">
              <Segmented
                label={text.language}
                value={lang}
                options={[
                  { value: "en", label: "EN" },
                  { value: "it", label: "IT" },
                ]}
                onChange={setLang}
              />
              <Segmented
                label={text.theme}
                value={theme}
                options={[
                  { value: "auto", label: text.themes.auto },
                  { value: "light", label: text.themes.light },
                  { value: "dark", label: text.themes.dark },
                ]}
                onChange={setTheme}
              />
              <p className="demo-status">
                <span className="demo-status__dot" aria-hidden="true" />
                {text.offline}
              </p>
            </div>
          </aside>
          <main ref={main} className="demo-main" tabIndex={-1} aria-labelledby="demo-section-title">
            <header className="demo-head">
              <p className="demo-head__path">
                @alixai/react <span aria-hidden="true">/</span> {text.sections[section].nav}
              </p>
              <h1 id="demo-section-title" className="demo-head__title">
                {text.sections[section].title}
              </h1>
              <p className="demo-head__lead">{text.sections[section].lead}</p>
            </header>
            <div key={section} className="demo-section">
              <SectionView id={section} />
            </div>
          </main>
          <Events title={text.events} events={events} onDismiss={(id) => setEvents((current) => current.filter((event) => event.id !== id))} />
        </div>
      </AlixProvider>
    </ShellContext.Provider>
  );
};
