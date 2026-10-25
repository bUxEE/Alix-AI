# Assign a technician

Capability `workorders.assign`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Assign a technician of the caller's site to a work order that is not closed, optionally with a note. The assignment is prepared first, its exact effects are shown, and it is committed only after the user confirms. Committing sets the assignee and sends a notification to the technician.

Scope: workspace (one site). Level 2, effect class external.

Operations: `validate` is `validateAssignTechnician`, `prepare` is `prepareAssignTechnician`, `commit` is `commitAssignTechnician`, `status` is `statusAssignTechnician`.

## Fields

### Field: work_order

Type entity_ref of work_order, required. The work order to assign.

A mention is resolved to a work order record by the application (`resolveWorkOrder`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: technician

Type entity_ref of technician, required. The technician who takes the work.

A mention is resolved to a technician record by the application (`resolveTechnician`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: note

Type string, optional, length 0 to 200. A note for the technician, included in the notification. Optional free text.

## Validation rules

### Rule: work_order_open

The work order must not be closed. A violation is reported as `conflict` on work_order.

### Rule: technician_assignable

The technician must be active and have fewer than 5 open work orders. A violation is reported as `conflict` on technician.

### Rule: already_assigned

The work order must not already be assigned to this technician. A violation is reported as `conflict` on work_order, technician.

### Rule: note_length

The note must be at most 200 characters of plain text. A violation is reported as `validation_failed` on note.

## Permissions

### Permission: workorders.assign

Allows assigning technicians to the work orders of the caller's site. Granted to supervisor only; checked by the host on every call.

## Failure states

### Failure: permission_denied

The principal's role may not use this capability.

The user is told: "You are not allowed to do that, so nothing was changed."

### Failure: scope_violation

A record named is outside the principal's access (another site, another tenant, or work the role may not see), or does not exist. The application does not say which.

The user is told: "I cannot do that: the record is not available to you. Nothing was changed."

### Failure: validation_failed

A value is missing, malformed or out of bounds. Nothing was changed.

The user is told: "I could not assign that: the note can be at most 200 characters. Nothing was assigned."

### Failure: conflict

A business rule is violated by the current state of the records. Nothing was changed.

The user is told: "I could not assign that: the work order is closed or already assigned to that technician, or the technician is inactive or already has 5 open work orders. Nothing was assigned."

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

### Outcome: assigned

What the user is told when the outcome is assigned: Done. Work order {work_order_id} is assigned to {technician_name}. The technician was notified (reference {notification_reference}).
