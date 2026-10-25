import type { ReactElement } from "react";
import { Icon } from "./Icon";
import type { DemoEvent } from "./context";

// What the host's callbacks received (onAction, onOpenCitation, onContinueInApp, saves): short notes at the bottom of the screen.
// They leave on their own; the list is a polite live region.

export const Events = ({ title, events, onDismiss }: { title: string; events: readonly DemoEvent[]; onDismiss: (id: number) => void }): ReactElement => (
  <section className="demo-events" aria-label={title} aria-live="polite">
    {events.map((event) => (
      <div key={event.id} className="demo-event">
        <code className="demo-event__name">{event.name}</code>
        <span className="demo-event__detail">{event.detail}</span>
        <button type="button" className="demo-icon-button" aria-label="Dismiss" onClick={() => onDismiss(event.id)}>
          <Icon name="close" size={14} />
        </button>
      </div>
    ))}
  </section>
);
