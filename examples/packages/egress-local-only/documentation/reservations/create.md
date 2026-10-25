# Reserve a title

Reserves a title for pickup at the user's branch. The reservation is prepared first, its exact effects are shown, and it is committed only after the user confirms.

## Field title

Required reference to a catalogue title. Resolved by the host; several matches trigger a clarification.

## Field pickup date

Required date in the branch time zone: today or later, at most 60 days ahead.

## Field notes

Optional free text up to 300 characters for the desk. Treated as sensitive and withheld from the model by policy.

## Rule pickup date

The pickup date must be between today and 60 days ahead, otherwise `validation_failed`.

## Rule title available

A copy must be free at the branch on the pickup date, otherwise `conflict`.

## Permission reservations write

Held by members for their own branch. Without it the host returns `permission_denied` at prepare.

## Failure permission denied

The principal may not reserve at this branch. Nothing was reserved.

## Failure validation failed

The pickup date is out of range. Nothing was reserved.

## Failure conflict

No copy is free on the pickup date. Nothing was reserved.

## Failure stale

Stock changed after prepare. The preparation is void and nothing was reserved; it can be prepared again.

## Failure expired

The prepared reservation was not confirmed within 10 minutes or was cancelled. Nothing was reserved.

## Failure uncertain outcome

The commit response was lost. The reservation status is queried before any retry, so it cannot be made twice.

## Risk

A reservation holds stock and notifies the desk, so it is a consequential action: prepared first, then committed.

## Confirmation

The user must confirm the exact reservation (title, branch, date) before commit. The confirmation expires after 10 minutes.

## Outcome reserved

The user sees the title, the pickup date and the reservation reference to quote at the desk.
