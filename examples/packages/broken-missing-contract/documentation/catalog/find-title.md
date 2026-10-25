# Find a title

Searches the catalogue of the user's branch. Read-only; nothing is changed.

## Field query

Words from the title, author or subject. Required, 2 to 200 characters. A blank query is rejected.

## Field available only

Optional boolean, default false. When true only titles with a free copy at the branch are listed.

## Field max results

Optional integer from 1 to 20, default 5. Caps how many titles are listed.

## Rule query not blank

The query must hold at least two characters that are not spaces, otherwise `validation_failed` is returned and no search runs.

## Permission catalog read

Held by members and visitors. Without it the host refuses the search with `permission_denied`.

## Failure validation failed

The query was blank or too short. Nothing was searched; the user is asked for a longer query.

## Failure unavailable

The catalogue service could not be reached. No result is shown and none is guessed.

## Risk

A read-only search of low-sensitivity catalogue data. Risk level 0: it runs after authorization with no confirmation.

## Confirmation

No confirmation is needed because the capability changes nothing.

## Outcome found

The user sees how many titles matched and the list with availability.

## Outcome none found

The user is told that no title matched the words they gave.
