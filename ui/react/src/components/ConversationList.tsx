import { useId } from "react";
import type { ReactElement } from "react";
import { formatDateTime, useAlixIntl } from "../i18n/context.js";

// The list of a person's conversations. It is data in, events out: the host decides where the list comes from and what
// switching does (CopilotKit's thread drawer and the gateway's session list are the usual sources).

export interface AlixConversationItem {
  readonly id: string;
  readonly title?: string;
  /** ISO 8601 time of the last activity. */
  readonly updatedAt?: string;
}

export interface AlixConversationListProps {
  items: readonly AlixConversationItem[];
  /** The conversation on screen. */
  activeId?: string;
  /** The person picked a conversation. */
  onSelect: (id: string) => void;
  /** The person asked for a new conversation. The button is shown only when this is given. */
  onNew?: () => void;
  title?: string;
}

export const AlixConversationList = ({ items, activeId, onSelect, onNew, title }: AlixConversationListProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.conversations;
  const titleId = useId();
  return (
    <nav className="alix-conversations" data-alixai="conversations" aria-labelledby={titleId}>
      <div className="alix-conversations__head">
        <h3 id={titleId} className="alix-conversations__title">
          {title ?? text.title}
        </h3>
        {onNew !== undefined && (
          <button type="button" className="alix-conversations__new" onClick={onNew}>
            {text.new}
          </button>
        )}
      </div>
      {items.length === 0 ? (
        <p className="alix-conversations__empty">{text.empty}</p>
      ) : (
        <ul className="alix-conversations__list" aria-label={text.list}>
          {items.map((item) => (
            <li key={item.id}>
              <button type="button" className="alix-conversations__item" aria-current={item.id === activeId ? "true" : undefined} onClick={() => onSelect(item.id)}>
                <span>{item.title ?? text.untitled}</span>
                {item.updatedAt !== undefined && (
                  <time className="alix-conversations__when" dateTime={item.updatedAt}>
                    {formatDateTime(intl, item.updatedAt)}
                  </time>
                )}
              </button>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
};
