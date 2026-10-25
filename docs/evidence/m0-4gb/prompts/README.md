# Real prompts of the M1-14 dialogue engine

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

`engine-prompts.json` holds the 19 chat requests (`providers.ChatRequest` as JSON: messages, output constraint, max_toks) that
`internal/dialogue` builds for typical turns: 1, 2, 3, 5, 8 and 12 shortlisted capabilities, with and without a goal and four turn
rows (English; the 5-capability case also in Italian), plus the five calls of the demo application (two proposals, three responses).
The estimated sizes (`est_toks`) reproduce `docs/efficiency/M1-14.md` exactly (763, 871, 1076, 1272, 334, 369, 330), so these are the
prompts that review sized.

They were produced by a throw-away test, `dump-engine-prompts.go.txt` (a `.txt` so it is not compiled here), run on a **copy** of
the tree made with `git archive HEAD` in a scratch directory, never added to `internal/dialogue`:

```bash
git archive HEAD | tar -x -C <scratch>/tree
cp docs/evidence/m0-4gb/prompts/dump-engine-prompts.go.txt <scratch>/tree/internal/dialogue/zz_m015_dump_test.go
cd <scratch>/tree && M015_DUMP_DIR=<scratch>/dump CGO_ENABLED=0 go test -count=1 -run TestM015DumpPrompts -v ./internal/dialogue/
```

`scripts/measure-prompts.mjs` sends each one to a running LocalAI and records the runtime's own token counts
(`measured-prompts-<model>.json` next to the run's samples).
