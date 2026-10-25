import { useId, useRef } from "react";
import type { KeyboardEvent, ReactElement } from "react";

// A small radio group drawn as a segmented control. Arrow keys move the choice, as a radio group does.

export interface SegmentedOption<T extends string> {
  readonly value: T;
  readonly label: string;
}

export interface SegmentedProps<T extends string> {
  readonly label: string;
  readonly value: T;
  readonly options: readonly SegmentedOption<T>[];
  readonly onChange: (value: T) => void;
  /** Hide the visible label (it stays the group's accessible name). */
  readonly hideLabel?: boolean;
}

export const Segmented = <T extends string>({ label, value, options, onChange, hideLabel = false }: SegmentedProps<T>): ReactElement => {
  const labelId = useId();
  const group = useRef<HTMLDivElement>(null);
  const move = (event: KeyboardEvent<HTMLDivElement>): void => {
    const step = event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : event.key === "ArrowLeft" || event.key === "ArrowUp" ? -1 : 0;
    if (step === 0) return;
    event.preventDefault();
    const index = options.findIndex((option) => option.value === value);
    const next = options[(index + step + options.length) % options.length];
    if (next === undefined) return;
    onChange(next.value);
    group.current?.querySelector<HTMLButtonElement>(`[data-value="${next.value}"]`)?.focus();
  };
  return (
    <div className="demo-segmented">
      <span id={labelId} className={hideLabel ? "demo-visually-hidden" : "demo-segmented__label"}>
        {label}
      </span>
      <div ref={group} className="demo-segmented__track" role="radiogroup" aria-labelledby={labelId} onKeyDown={move}>
        {options.map((option) => {
          const checked = option.value === value;
          return (
            <button
              key={option.value}
              type="button"
              role="radio"
              aria-checked={checked}
              tabIndex={checked ? 0 : -1}
              className="demo-segmented__option"
              data-value={option.value}
              onClick={() => onChange(option.value)}
            >
              {option.label}
            </button>
          );
        })}
      </div>
    </div>
  );
};
