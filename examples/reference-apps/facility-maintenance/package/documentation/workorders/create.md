# Create a work order

Capability `workorders.create`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Raise a new work order against an active asset of the caller's site. The work order starts open and unassigned. It is a reversible change that needs no separate confirmation: the application shows a concise summary and the order can be closed or reassigned afterwards.

Scope: workspace (one site). Level 1, effect class reversible.

Operations: `validate` is `validateCreateWorkOrder`, `prepare` is `prepareCreateWorkOrder`, `commit` is `commitCreateWorkOrder`, `status` is `statusCreateWorkOrder`.

## Fields

### Field: asset

Type entity_ref of asset, required. The asset the work is for.

A mention is resolved to a asset record by the application (`resolveAsset`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: title

Type string, required, length 5 to 120. A short statement of the problem or the job.

### Field: description

Type string, optional, length 0 to 500. More detail about the problem, in the reporter's words. Optional free text.

### Field: priority

Type enum (low, normal, high, urgent), optional, default normal. How soon it must be dealt with. Defaults to normal.

## Validation rules

### Rule: title_length

The title must be 5 to 120 characters of plain text. A violation is reported as `validation_failed` on title.

### Rule: asset_active

The asset must still be active: a decommissioned asset takes no new work. A violation is reported as `conflict` on asset.

### Rule: no_duplicate_open

The asset must not already have an open work order with the same title. A violation is reported as `conflict` on asset, title.

## Permissions

### Permission: workorders.create

Allows raising work orders against the assets of the caller's site. Granted to technician and supervisor; checked by the host on every call.

## Failure states

### Failure: permission_denied

The principal's role may not use this capability.

The user is told: "You are not allowed to do that, so nothing was changed."

### Failure: scope_violation

A record named is outside the principal's access (another site, another tenant, or work the role may not see), or does not exist. The application does not say which.

The user is told: "I cannot do that: the record is not available to you. Nothing was changed."

### Failure: validation_failed

A value is missing, malformed or out of bounds. Nothing was changed.

The user is told: "I could not create that: the title must be 5 to 120 characters. Nothing was created."

### Failure: conflict

A business rule is violated by the current state of the records. Nothing was changed.

The user is told: "I could not create that: the asset is decommissioned, or it already has an open work order with this title. Nothing was created."

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

### Level 1

Level 1 (reversible low-impact change): the application shows a concise summary and commits immediately; the change can be undone by a later work order action. Effect class reversible.

## Confirmation

### Confirmation: none

No separate confirmation is asked: the application commits right after the concise summary. The host still reauthorises the call and limits it to the caller's own site.

A preparation is honoured for 120 seconds and is invalidated by: record_revision. Commit is idempotent for 86400 seconds: a repeat returns the original receipt.

## Outcomes

### Outcome: created

What the user is told when the outcome is created: Done. Work order {work_order_id} was created for {asset_name}: {work_order_title}, priority {priority}.
