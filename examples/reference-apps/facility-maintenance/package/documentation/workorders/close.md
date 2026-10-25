# Close a work order

Capability `workorders.close`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Close a work order with a resolution, the parts used and the labour hours. The closure is prepared first, its exact effects are shown (including the stock each part falls to and the parts cost), and it is committed only after the user confirms. Committing closes the work order and reduces the site's stock of every part used.

Scope: workspace (one site). Level 2, effect class external.

Operations: `validate` is `validateCloseWorkOrder`, `prepare` is `prepareCloseWorkOrder`, `commit` is `commitCloseWorkOrder`, `status` is `statusCloseWorkOrder`.

## Fields

### Field: work_order

Type entity_ref of work_order, required. The work order to close.

A mention is resolved to a work order record by the application (`resolveWorkOrder`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: resolution

Type string, required, length 3 to 500. What was done to resolve it, in the technician's words.

### Field: parts_used

Type list of string, optional, at most 20 items. The parts consumed, each written PART-NUMBER:QUANTITY, for example FLT-204:2. Leave it out when no parts were used.

### Field: labor_hours

Type number, optional, from 0 to 24. The hours of labour spent, with at most two decimals. Leave it out when not recorded.

## Validation rules

### Rule: work_order_closable

The work order must not already be closed. A violation is reported as `conflict` on work_order.

### Rule: resolution_length

The resolution must be 3 to 500 characters of plain text. A violation is reported as `validation_failed` on resolution.

### Rule: parts_format

Each part must be written PART-NUMBER:QUANTITY with a quantity of 1 to 999, and each part number may appear only once. A violation is reported as `validation_failed` on parts_used.

### Rule: parts_known

Every part number must be in the stock catalogue of the caller's site. A violation is reported as `validation_failed` on parts_used.

### Rule: parts_in_stock

Every part must be in stock in the quantity used. A violation is reported as `conflict` on parts_used.

### Rule: labor_hours_in_range

The labour hours must be from 0 to 24 with at most two decimals. A violation is reported as `validation_failed` on labor_hours.

## Permissions

### Permission: workorders.close

Allows closing the work orders the caller may see at their site. Granted to technician and supervisor; checked by the host on every call, which also limits a technician to their own work.

## Failure states

### Failure: permission_denied

The principal's role may not use this capability.

The user is told: "You are not allowed to do that, so nothing was changed."

### Failure: scope_violation

A record named is outside the principal's access (another site, another tenant, or work the role may not see), or does not exist. The application does not say which.

The user is told: "I cannot do that: the record is not available to you. Nothing was changed."

### Failure: validation_failed

A value is missing, malformed or out of bounds. Nothing was changed.

The user is told: "I could not close that: check the resolution, write each part as PART-NUMBER:QUANTITY with parts from this site's catalogue, and give 0 to 24 labour hours. Nothing was closed."

### Failure: conflict

A business rule is violated by the current state of the records. Nothing was changed.

The user is told: "I could not close that: the work order is already closed, or a part is not in stock in that quantity. Nothing was closed."

### Failure: stale

A record changed after the action was prepared, so the preparation is no longer valid. Nothing was changed; it can be prepared again.

The user is told: "A record changed after I prepared this, so I did not apply it. Nothing was changed; I can prepare it again."

### Failure: expired

The prepared action was not confirmed in time, or was withdrawn. Nothing was changed.

The user is told: "This was not confirmed in time, or it was withdrawn, so nothing was changed."

### Failure: uncertain_outcome

The commit response was lost. Whether the change was applied is established through the status operation before anything is retried.

The user is told: "I could not confirm whether this was applied. I will check before doing anything else, so please do not ask for it again yet."

### Failure: missing_fields

A required field was not supplied. Nothing was changed.

The user is told: "I still need a few details before I can do that. Nothing was changed."

### Failure: unavailable

The application's data store cannot be reached. Nothing was changed.

The user is told: "The maintenance system is not reachable right now, so nothing was changed."

## Risk level

### Level 2

Level 2 (consequential or external side effect): the change is prepared, its exact effects are shown and it is committed only after the user explicitly confirms. Effect class external.

## Confirmation

### Confirmation: explicit

The user must confirm the exact prepared effects before anything is committed. The confirmation is bound to the preparation, the principal, the session and the data versions it relied on.

A preparation is honoured for 600 seconds and is invalidated by: record_revision, permission_change, user_change. Commit is idempotent for 86400 seconds: a repeat returns the original receipt.

## Outcomes

### Outcome: closed

What the user is told when the outcome is closed: Done. Work order {work_order_id} is closed ({closed_at}). Parts cost: {parts_cost}.
