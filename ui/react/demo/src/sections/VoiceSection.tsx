import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import { AlixTranscriptEditor, AlixVoiceButton, AlixVoiceMode } from "@alixai/react";
import { demoVoiceOptions } from "@alixai/demo-mock";
import { voice } from "@alixai/sdk";
import { createSimulatedMicrophone } from "../lib/microphone";
import { useShell } from "../shell/context";
import { Segmented } from "../shell/Segmented";

type ButtonState = "idle" | "listening" | "processing";

/** The talk button on its own: it owns no audio, so the page plays the states it reports. */
const ButtonSpecimen = (): ReactElement => {
  const { text, notify } = useShell();
  const [mode, setMode] = useState<"toggle" | "push-to-talk">("toggle");
  const [state, setState] = useState<ButtonState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const stop = (): void => {
    setState("processing");
    notify("onStop", mode);
    timer.current = setTimeout(() => setState("idle"), 900);
  };
  return (
    <section className="demo-frame demo-frame--pad demo-stack" aria-labelledby="demo-voice-button">
      <div className="demo-stack demo-stack--tight">
        <h2 id="demo-voice-button" className="demo-frame__title">
          {text.voice.buttonTitle}
        </h2>
        <p className="demo-muted">{text.voice.buttonLead}</p>
      </div>
      <Segmented
        label={text.voice.mode}
        hideLabel
        value={mode}
        options={[
          { value: "toggle", label: "toggle" },
          { value: "push-to-talk", label: "push-to-talk" },
        ]}
        onChange={setMode}
      />
      <div className="demo-row demo-row--center">
        <AlixVoiceButton
          state={state}
          mode={mode}
          size="lg"
          onStart={() => {
            setState("listening");
            notify("onStart", mode);
          }}
          onStop={stop}
          onCancel={() => {
            setState("idle");
            notify("onCancel", mode);
          }}
        />
        <p className="demo-muted">
          {text.voice.state}: <code>{state}</code>
        </p>
      </div>
    </section>
  );
};

/** The transcript editor on its own: what the recogniser heard, corrected before it is sent. */
const EditorSpecimen = (): ReactElement => {
  const { text, notify, lang } = useShell();
  const [round, setRound] = useState(0);
  return (
    <section className="demo-frame demo-frame--pad demo-stack" aria-labelledby="demo-voice-editor">
      <div className="demo-stack demo-stack--tight">
        <h2 id="demo-voice-editor" className="demo-frame__title">
          {text.voice.editorTitle}
        </h2>
        <p className="demo-muted">{text.voice.editorLead}</p>
      </div>
      <AlixTranscriptEditor
        key={`${lang}:${round}`}
        defaultValue={text.voice.sampleTranscript}
        original={text.voice.sampleTranscript}
        onConfirm={(value) => {
          notify("onConfirm", value);
          setRound((current) => current + 1);
        }}
        onDiscard={() => {
          notify("onDiscard", text.voice.discarded);
          setRound((current) => current + 1);
        }}
      />
    </section>
  );
};

export const VoiceSection = (): ReactElement => {
  const { lang, text, threadId, notify } = useShell();
  const [simulated, setSimulated] = useState(true);
  const [speak, setSpeak] = useState(true);
  const [generation, setGeneration] = useState(0);
  const microphone = useMemo(() => (simulated ? createSimulatedMicrophone() : undefined), [simulated]);
  // A voice session per language and per "speak" choice: the host makes it (it has the token provider and the socket factory), and
  // closes it when it is replaced. `generation` makes a fresh one after the previous one ended for good.
  const [session, setSession] = useState<voice.VoiceSession | undefined>(undefined);
  useEffect(() => {
    const created = voice.createVoiceSession({ ...demoVoiceOptions(threadId, { language: lang }), speak });
    setSession(created);
    return () => {
      created.close();
    };
  }, [threadId, lang, speak, generation]);

  return (
    <div className="demo-stack demo-stack--loose">
      <div className="demo-toolbar">
        <label className="demo-check" title={text.voice.simulatedHint}>
          <input type="checkbox" className="no-validation" checked={simulated} onChange={(event) => setSimulated(event.target.checked)} />
          {text.voice.simulated}
        </label>
        <label className="demo-check">
          <input type="checkbox" className="no-validation" checked={speak} onChange={(event) => setSpeak(event.target.checked)} />
          {text.voice.speak}
        </label>
        <p className="demo-toolbar__meta">{text.voice.simulatedHint}</p>
      </div>
      <div className="demo-frame demo-voice">
        <AlixVoiceMode
          key={`${simulated}`}
          {...(session !== undefined && { session })}
          autoConnect={session !== undefined}
          {...(microphone !== undefined && { microphone })}
          {...(!speak && { player: null })}
          language={lang}
          onTurn={(turn) => notify("onTurn", `${turn.act}${turn.confirmationRequired ? ", confirmation on screen" : ""}`)}
          onCorrect={(value) => notify("onCorrect", value)}
          onRestart={() => setGeneration((current) => current + 1)}
          onError={(error) => notify("onError", `${error.code}: ${error.message}`)}
        />
      </div>
      <div className="demo-pair">
        <ButtonSpecimen />
        <EditorSpecimen />
      </div>
    </div>
  );
};
