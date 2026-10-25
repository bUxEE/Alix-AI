import { useEffect, useState } from "react";
import type { ReactElement, ReactNode } from "react";
import {
  AlixActionCard,
  AlixChartBlock,
  AlixCitationList,
  AlixClarificationForm,
  AlixConfirmationOutcome,
  AlixConfirmationPanel,
  AlixConfirmationSheet,
  AlixContinueInApp,
  AlixDiagramBlock,
  AlixFileList,
  AlixProgressLine,
  AlixReceiptCard,
  AlixTableBlock,
  AlixVoiceOrb,
} from "@alixai/react";
import type { VoiceOrbState } from "@alixai/react";
import { DEMO_PATHS, requestJson, toAttachment, useResource } from "../lib/api";
import type { DemoFileRef } from "../lib/api";
import { actionCardBlock, chartBlock, choicesBlock, citations, diagramBlock, interrupt, panelBlock, receipt, tableBlock } from "../lib/fixtures";
import type { Mark } from "../lib/fixtures";
import { startSimulatedLevel } from "../lib/microphone";
import { renderPreview } from "../lib/preview";
import { useShell } from "../shell/context";
import { Segmented } from "../shell/Segmented";

// Every kit component on its own, with fixed data. The label sits outside the specimen, so the component is seen as it ships.

const Specimen = ({ name, note, controls, wide, children }: { name: string; note: string; controls?: ReactNode; wide?: boolean; children: ReactNode }): ReactElement => (
  <figure className={wide === true ? "demo-specimen demo-specimen--wide" : "demo-specimen"}>
    <div className="demo-specimen__stage">{children}</div>
    <figcaption className="demo-specimen__caption">
      <code className="demo-specimen__name">{`<${name}>`}</code>
      <span className="demo-specimen__note">{note}</span>
      {controls}
    </figcaption>
  </figure>
);

const ORB_STATES: readonly VoiceOrbState[] = ["idle", "listening", "thinking", "speaking", "muted", "error"];

/** The voice orb in each state, side by side. Listening and speaking follow a simulated voice level; its own state re-renders only this. */
const OrbStates = (): ReactElement => {
  const [level, setLevel] = useState(0);
  useEffect(() => startSimulatedLevel(setLevel), []);
  return (
    <ul className="demo-orbs">
      {ORB_STATES.map((state) => (
        <li key={state} className="demo-orbs__item">
          <AlixVoiceOrb state={state} level={state === "listening" || state === "speaking" ? level : 0} size={112} />
          <code className="demo-specimen__name">{state}</code>
        </li>
      ))}
    </ul>
  );
};

/** A progress line that keeps moving: its own interval, so nothing else re-renders. */
const LiveProgress = (): ReactElement => {
  const [percent, setPercent] = useState(12);
  useEffect(() => {
    const timer = setInterval(() => setPercent((current) => (current >= 100 ? 8 : Math.min(100, current + 11))), 700);
    return () => clearInterval(timer);
  }, []);
  return <AlixProgressLine progress={{ stage: "Exporting the Q3 report", state: percent >= 100 ? "completed" : "running", percent }} />;
};

const SheetSpecimen = (): ReactElement => {
  const { text, notify } = useShell();
  const [round, setRound] = useState(0);
  const [raised, setRaised] = useState(false);
  const [answer, setAnswer] = useState<string | undefined>(undefined);
  const decide = (value: string): void => {
    setAnswer(value);
    notify(value === text.blocks.approved ? "onApprove" : "onDecline", '{"approved": ' + String(value === text.blocks.approved) + "}");
  };
  return (
    <Specimen
      name="AlixConfirmationSheet"
      note="The interrupt of a risk 2 action. It reports one answer; the host resumes the run with {approved}."
      controls={
        answer !== undefined ? (
          <button
            type="button"
            className="demo-link"
            onClick={() => {
              setAnswer(undefined);
              setRound((current) => current + 1);
            }}
          >
            {text.blocks.reset}
          </button>
        ) : undefined
      }
    >
      {raised ? (
        <AlixConfirmationSheet key={round} interrupt={interrupt()} onApprove={() => decide(text.blocks.approved)} onDecline={() => decide(text.blocks.declined)} />
      ) : (
        // The sheet takes focus when it appears, as an interrupt should: it appears on request, not on page load.
        <div className="demo-empty">
          <button type="button" className="demo-button" data-variant="primary" onClick={() => setRaised(true)}>
            {text.blocks.raise}
          </button>
        </div>
      )}
    </Specimen>
  );
};

export const BlocksSection = (): ReactElement => {
  const { text, notify } = useShell();
  const [mark, setMark] = useState<Mark>("bar");
  const [status, setStatus] = useState<"committed" | "queued" | "partial">("committed");
  const files = useResource((signal) => requestJson<{ files: DemoFileRef[] }>(DEMO_PATHS.fileList, { signal }).then((body) => body.files.map(toAttachment)));

  return (
    <div className="demo-gallery">
      <Specimen
        name="AlixVoiceOrb"
        note="Every state side by side. Listening and speaking follow a simulated voice; with reduced motion the orb holds still and keeps size and brightness."
        wide
      >
        <OrbStates />
      </Specimen>
      <Specimen
        name="AlixChartBlock"
        note="Declarative data drawn with the theme's chart colours. The same data is always one click away as a table."
        controls={
          <Segmented
            label="mark"
            hideLabel
            value={mark}
            options={(["bar", "line", "area", "point", "pie"] as const).map((value) => ({ value, label: value }))}
            onChange={setMark}
          />
        }
      >
        <AlixChartBlock block={chartBlock(mark)} />
      </Specimen>
      <Specimen name="AlixTableBlock" note="Text, number, date and yes/no columns. Every cell is text: nothing in a payload becomes markup.">
        <AlixTableBlock block={tableBlock()} />
      </Specimen>
      <SheetSpecimen />
      <Specimen name="AlixConfirmationPanel" note="The confirmation_panel block: what was prepared stays in the transcript after the sheet is gone.">
        <AlixConfirmationPanel block={panelBlock("explicit")} />
      </Specimen>
      <Specimen name="AlixConfirmationPanel + AlixContinueInApp" note="A risk 3 step-up: the chat cannot complete it, the application can.">
        <div className="demo-stack">
          <AlixConfirmationPanel block={panelBlock("step_up")} />
          <AlixContinueInApp request={{ preparationId: "prep_gallery_step_up", summary: "End the Brightline contract" }} onContinue={(request) => notify("onContinueInApp", request.summary ?? "")} />
        </div>
      </Specimen>
      <Specimen
        name="AlixReceiptCard"
        note="The application's own facts about what happened. The model's wording never decides the outcome shown."
        controls={
          <Segmented
            label="status"
            hideLabel
            value={status}
            options={(["committed", "queued", "partial"] as const).map((value) => ({ value, label: value }))}
            onChange={setStatus}
          />
        }
      >
        <div className="demo-stack">
          <AlixReceiptCard receipt={receipt(status)} />
          <AlixConfirmationOutcome status="rejected" />
        </div>
      </Specimen>
      <Specimen name="AlixActionCard" note="The permitted next actions go back to the host (onAction); the card commits nothing.">
        <AlixActionCard block={actionCardBlock()} onAction={(action) => notify("onAction", action.label ?? action.action)} />
      </Specimen>
      <Specimen name="AlixClarificationForm" note="The question, its choices and optional free text. A form is data, not executable UI.">
        <AlixClarificationForm block={choicesBlock()} onChoose={(choiceId) => notify("onChoose", choiceId)} onFreeText={(value) => notify("onFreeText", value)} />
      </Specimen>
      <Specimen name="AlixCitationList" note="Version, locator and what each source supports. Only an available source can be opened.">
        <AlixCitationList citations={citations()} onOpenCitation={(citation) => notify("onOpenCitation", citation.display_name)} />
      </Specimen>
      <Specimen name="AlixDiagramBlock" note="A diagram needs a renderer only the host can choose, so the kit shows its source and description.">
        <AlixDiagramBlock block={diagramBlock()} />
      </Specimen>
      <Specimen name="AlixProgressLine" note="The alixai.progress activity, shown when the chat is given showProgress.">
        <LiveProgress />
      </Specimen>
      <Specimen name="AlixFileList" note="Attachments with the host's previews (renderPreview). PDFs have no safe preview, so they show their kind.">
        {files.data === undefined ? <div className="demo-viewer__skeleton" aria-hidden="true" /> : <AlixFileList files={files.data.slice(0, 5)} renderPreview={renderPreview} />}
      </Specimen>
    </div>
  );
};
