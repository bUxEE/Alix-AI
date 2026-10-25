# Version matrix

`matrix.json` is the list of CopilotKit and AG-UI releases the adapter suite and the compatibility gate (`tests/copilotkit`, `@alixai/copilotkit-gate`) run against. It is data first: `packages` is a flat,
exact name-to-version map per entry, so `tests/copilotkit` (M3-28) pins its devDependencies from it, and `test/matrix.test.ts` fails
when the entry under test differs from what is installed.

| Entry | CopilotKit | AG-UI | How it runs |
| --- | --- | --- | --- |
| `copilotkit-1.76.0` (primary) | 1.76.0 | `@ag-ui/client` and `@ag-ui/core` 1.0.1 | in the workspace: `pnpm --filter @alixai/copilotkit-adapter test` and `pnpm --filter @alixai/copilotkit-gate test` |
| `copilotkit-1.77.0` | 1.77.0 | 1.0.1 | scratch install, release-age policy lifted for it |

The workspace stays on the newest release that passes pnpm's release-age policy. Releases before 1.76.0 depend on `@ag-ui/client`
0.0.59, which predates the interrupt outcome and resume of the AG-UI release AlixAI implements; they are not tested and not claimed.

## Running it

```bash
pnpm --filter @alixai/copilotkit-adapter compat                       # every entry
node compat/run.mjs --only copilotkit-1.77.0                          # one entry (from sdk/copilotkit-adapter)
node compat/run.mjs --list
```

For a non-primary entry the script copies the adapter, the gate, the SDK sources, the golden streams and the pages and source trees the gate reads into a scratch directory
under the system temp directory with the repository's layout, writes a workspace file listing both packages (it denies the `@scarf/scarf` install script), installs the
entry's packages and runs the adapter suite and then the gate there, with `ALIXAI_COMPAT_ENTRY` naming the entry. The summary prints one line per entry with the result of each suite,
and the exit code is non-zero when any suite of any entry fails. Nothing in the repository changes; `--keep` leaves the scratch directory.
The suite refuses every non-loopback connection; only the install step talks to the registry.

## Adding a release

Add an entry with the same keys as the others, run `node compat/run.mjs --only <id>`, and set `status` to `tested` only when it passes.
Record anything a release does differently under `notes`. Moving the **primary** entry means changing the pins in `package.json`
and running `pnpm install` first (`test/matrix.test.ts` checks both).

## Telemetry switches

`matrix.json` also records the switches that keep CopilotKit's telemetry off and the file of the pinned release that reads each one
(`telemetry.readBy`); `test/telemetry.test.ts` reads those files from `node_modules` and fails if a switch is no longer there.
