import { useEffect, useId, useRef, useState, type FormEvent, type ReactElement, type ReactNode } from "react";
import { useAlixText } from "../i18n/context.js";

// Memory proposal card and memory list. Host owns API calls; these components only report decisions.

export type MemoryProposalStatus = "pending" | "submitting" | "remembered" | "forgotten" | "expired" | "failed";

export interface MemoryProposal {
  readonly id: string;
  readonly text: string;
  readonly expiresAt?: string;
}

export interface AlixMemoryProposalProps {
  proposal: MemoryProposal;
  status?: MemoryProposalStatus;
  onConfirm: (id: string) => void | Promise<void>;
  onReject: (id: string) => void | Promise<void>;
  children?: ReactNode;
}

export const AlixMemoryProposal = ({
  proposal,
  status = "pending",
  onConfirm,
  onReject,
  children,
}: AlixMemoryProposalProps): ReactElement => {
  const text = useAlixText().memory;
  const [answered, setAnswered] = useState(false);
  const [clock, setClock] = useState(() => Date.now());
  const titleId = useId();
  const expiresMs = proposal.expiresAt === undefined ? Number.NaN : Date.parse(proposal.expiresAt);
  const expired = status === "expired" || (!Number.isNaN(expiresMs) && expiresMs <= clock);
  const open = status === "pending" || status === "failed";
  const busy = status === "submitting";
  const canAnswer = open && !expired && !answered && !busy;

  useEffect(() => {
    setAnswered(false);
  }, [proposal.id, status]);

  useEffect(() => {
    if (Number.isNaN(expiresMs) || expiresMs <= Date.now()) return undefined;
    const remaining = expiresMs - Date.now();
    if (remaining >= 2 ** 31 - 1) return undefined;
    const timer = setTimeout(() => setClock(Date.now()), remaining + 25);
    return () => clearTimeout(timer);
  }, [expiresMs]);

  const answer = (kind: "confirm" | "reject"): void => {
    if (!canAnswer) return;
    setAnswered(true);
    void (kind === "confirm" ? onConfirm(proposal.id) : onReject(proposal.id));
  };

  return (
    <section
      aria-busy={busy}
      aria-labelledby={titleId}
      className={`alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3${expired && open ? " alix-tone-danger" : ""}`}
    >
      <header className="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
        <h3
          className="alix-heading alix-ma-0"
          id={titleId}
        >
          {text.proposalTitle}
        </h3>
      </header>
      {children ?? <p className="alix-ma-0 alix-break-word">{proposal.text}</p>}
      {expired && open ? (
        <p
          className="alix-banner alix-tone-danger alix-ma-0 alix-text-sm"
          role="status"
        >
          {text.proposalExpired}
        </p>
      ) : null}
      {status === "failed" ? (
        <p
          className="alix-banner alix-tone-danger alix-ma-0 alix-text-sm"
          role="status"
        >
          {text.proposalFailed}
        </p>
      ) : null}
      {status === "remembered" || status === "forgotten" ? (
        <p
          className="alix-ma-0 alix-text-sm"
          role="status"
        >
          {status === "remembered" ? text.remembered : text.forgotten}
        </p>
      ) : null}
      {open || busy ? (
        <div className="alix-d-flex alix-flex-wrap alix-ga-2">
          <button
            className="alix-btn alix-btn-primary"
            disabled={!canAnswer || busy}
            type="button"
            onClick={() => answer("confirm")}
          >
            {busy ? text.submitting : text.remember}
          </button>
          <button
            className="alix-btn"
            disabled={!canAnswer || busy}
            type="button"
            onClick={() => answer("reject")}
          >
            {text.forget}
          </button>
        </div>
      ) : null}
    </section>
  );
};

export interface MemoryListItem {
  readonly id: string;
  readonly text: string;
  readonly status: "proposed" | "active" | "rejected";
}

export interface AlixMemoryListProps {
  items?: readonly MemoryListItem[];
  title?: string;
  loading?: boolean;
  failed?: boolean;
  editable?: boolean;
  onEdit?: (item: MemoryListItem) => void;
  onDelete?: (id: string) => void;
  onSave?: (item: MemoryListItem, text: string) => void;
  onRetry?: () => void;
  renderRow?: (item: MemoryListItem) => ReactNode;
}

export const AlixMemoryList = ({
  items,
  title,
  loading = false,
  failed = false,
  editable = true,
  onEdit,
  onDelete,
  onSave,
  onRetry,
  renderRow,
}: AlixMemoryListProps): ReactElement => {
  const text = useAlixText().memory;
  const loadedOnce = useRef(items !== undefined);
  if (items !== undefined) loadedOnce.current = true;
  const firstLoad = loading && !loadedOnce.current;
  const refreshing = loading && loadedOnce.current;
  const [editingId, setEditingId] = useState<string | undefined>(undefined);
  const [draft, setDraft] = useState("");

  const startEdit = (item: MemoryListItem): void => {
    setEditingId(item.id);
    setDraft(item.text);
    onEdit?.(item);
  };

  const save = (event: FormEvent, item: MemoryListItem): void => {
    event.preventDefault();
    const next = draft.trim();
    if (next === "") return;
    onSave?.(item, next);
    setEditingId(undefined);
    setDraft("");
  };

  return (
    <section
      aria-busy={loading}
      aria-label={text.listTitle}
      className="alix-root alix-d-flex alix-flex-column alix-ga-3 alix-relative"
    >
      <header>
        <h2 className="alix-heading alix-ma-0">{title ?? text.listTitle}</h2>
      </header>
      {firstLoad ? (
        <ul className="alix-reset-list alix-d-flex alix-flex-column alix-ga-2">
          {[1, 2, 3].map((n) => (
            <li
              key={n}
              className="alix-card alix-pa-3"
            >
              <div className="alix-skeleton" style={{ width: "70%", height: "1rem" }} />
            </li>
          ))}
        </ul>
      ) : failed && !loadedOnce.current ? (
        <div
          className="alix-banner alix-tone-danger alix-d-flex alix-align-center alix-justify-between alix-ga-2"
          role="alert"
        >
          <span>{text.listFailed}</span>
          <button
            className="alix-btn"
            type="button"
            onClick={() => onRetry?.()}
          >
            {text.retry}
          </button>
        </div>
      ) : items !== undefined && items.length === 0 ? (
        <p className="alix-text-muted alix-ma-0">{text.listEmpty}</p>
      ) : items !== undefined ? (
        <ul className="alix-reset-list alix-d-flex alix-flex-column alix-ga-2">
          {items.map((item) => (
            <li
              key={item.id}
              className="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
            >
              {renderRow !== undefined ? (
                renderRow(item)
              ) : editingId === item.id ? (
                <form
                  className="alix-d-flex alix-flex-column alix-ga-2"
                  onSubmit={(event) => save(event, item)}
                >
                  <textarea
                    className="alix-input"
                    rows={3}
                    value={draft}
                    onChange={(event) => setDraft(event.target.value)}
                  />
                  <div className="alix-d-flex alix-ga-2">
                    <button
                      className="alix-btn alix-btn-primary"
                      type="submit"
                    >
                      {text.save}
                    </button>
                    <button
                      className="alix-btn"
                      type="button"
                      onClick={() => setEditingId(undefined)}
                    >
                      {text.discard}
                    </button>
                  </div>
                </form>
              ) : (
                <>
                  <p className="alix-ma-0 alix-break-word">{item.text}</p>
                  <div className="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
                    <span className="alix-text-muted alix-text-sm">{text.status[item.status]}</span>
                    {editable ? (
                      <div className="alix-d-flex alix-ga-1">
                        <button
                          aria-label={text.edit}
                          className="alix-btn alix-btn-ghost"
                          type="button"
                          onClick={() => startEdit(item)}
                        >
                          {text.edit}
                        </button>
                        <button
                          aria-label={text.delete}
                          className="alix-btn alix-btn-ghost"
                          type="button"
                          onClick={() => onDelete?.(item.id)}
                        >
                          {text.delete}
                        </button>
                      </div>
                    ) : null}
                  </div>
                </>
              )}
            </li>
          ))}
        </ul>
      ) : null}
      {refreshing ? (
        <div
          aria-hidden="true"
          className="alix-section-loader"
        />
      ) : null}
    </section>
  );
};
