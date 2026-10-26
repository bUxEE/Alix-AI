# Ask about the product

Returns a read-only overview for a topic from the signed FAQ. Nothing is changed.

## Field topic

Words that name the feature, limit or how-to. Required, 2 to 200 characters. A blank topic is rejected.

## Field max results

Optional integer from 1 to 10, default 5. Caps how many FAQ snippets are considered.

## Rule topic not blank

The topic must hold at least two characters that are not spaces, otherwise `validation_failed` is returned and no overview runs.

## Permission help read

Held by users. Without it the host refuses the lookup with `permission_denied`.

## Failure validation failed

The topic was blank or too short. Nothing was looked up; the user is asked for a longer topic.

## Failure unavailable

The help service could not be reached. No answer is shown and none is guessed.

## Risk

A read-only FAQ overview of low-sensitivity help text. Risk level 0: it runs after authorization with no confirmation.

## Confirmation

No confirmation is needed because the capability changes nothing.

## Outcome found

The user sees how many FAQ items matched and a short summary.

## Outcome none found

The user is told that no FAQ entry matched the topic they gave.
