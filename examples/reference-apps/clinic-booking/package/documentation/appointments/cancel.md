# Cancel an appointment

Capability `appointments.cancel`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Cancels an upcoming appointment that the caller is allowed to see. The cancellation is prepared first, the exact effects are shown, and it is committed only after the user explicitly confirms. Committing releases the slot and queues a cancellation message to the patient's contact details.

Scope: workspace (one clinic site). Roles that may use it: receptionist, doctor, patient.

Operations: `validate` is the MCP tool `appointments_cancel_validate`; `prepare` is the MCP tool `appointments_cancel_prepare`; `commit` is the MCP tool `appointments_cancel_commit`; `status` is the MCP tool `appointments_cancel_status`.

A prepared action stays confirmable for 600 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the version of the records the action relies on (the snapshot of those records, not a response tag and not a site-wide counter).

## Fields

### Field: appointment

Type entity_ref of appointment, required. The upcoming appointment to cancel, named by its reference, its identifier, its local date or the patient's name.

A mention is resolved to a appointment record by the application (`resolve_appointment`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: reason

Type enum, optional, one of patient_request, provider_unavailable, scheduling_error, other. Why the appointment is cancelled.

## Validation rules

### Rule: appointment_active

The appointment must not already be cancelled. A violation is reported as `conflict` on appointment.

### Rule: appointment_not_started

An appointment that has started or passed cannot be cancelled. A violation is reported as `validation_failed` on appointment.

## Permissions

### Permission: appointments.cancel

Allows cancelling an upcoming appointment the caller may see and resolving appointments. Granted to: receptionist, doctor, patient. The host reauthorises every read, validate, prepare, commit and status call from the identity it receives and applies record-level visibility on top of the role, so holding the permission never reaches another clinic site or another clinic group.

## Failure states

### Failure: permission_denied

The principal's role may not cancel appointments.

The user is told: "You are not allowed to cancel appointments, so nothing was cancelled."

### Failure: scope_violation

The appointment is outside the principal's access (another site, another clinic group, another patient's or doctor's), or does not exist. The application does not say which.

The user is told: "I cannot cancel that: the appointment is not available to you. Nothing was cancelled."

### Failure: validation_failed

The appointment has started or passed, or a value is malformed. Nothing was cancelled.

The user is told: "I could not cancel that: an appointment that has started or passed cannot be cancelled. Nothing was cancelled."

### Failure: conflict

The appointment is already cancelled. Nothing was changed.

The user is told: "That appointment is already cancelled. Nothing was changed."

### Failure: stale

The appointment changed after the cancellation was prepared, so the preparation is no longer valid. Nothing was cancelled; it can be prepared again.

The user is told: "The appointment changed after I prepared this cancellation, so I did not cancel it. Nothing was cancelled; I can prepare it again."

### Failure: expired

The prepared cancellation was not confirmed in time, or was withdrawn. Nothing was cancelled.

The user is told: "The cancellation was not confirmed in time, or it was withdrawn, so nothing was cancelled."

### Failure: uncertain_outcome

The commit response was lost. Whether the appointment was cancelled is established through the status operation before anything is retried.

The user is told: "I could not confirm whether the appointment was cancelled. I will check before doing anything else, so please do not ask for it again yet."

### Failure: missing_fields

A required field (appointment) was not supplied. Nothing was cancelled.

The user is told: "I still need to know which appointment to cancel. Nothing was cancelled."

### Failure: unavailable

The clinic database cannot be reached. Nothing was cancelled.

The user is told: "The appointment system is not reachable right now, so nothing was cancelled."

## Risk level

### Level 2

Level 2 (consequential or external side effect): the action is prepared, its exact effects are shown, and it is committed only after explicit confirmation. Effect class: external. Committing cancels the appointment, frees the slot and sends a message to the patient, which is a side effect outside the appointment record. That is why the effect class is external and the level is 2.

## Confirmation rule

### Confirmation: explicit

The user must confirm the exact prepared cancellation (appointment reference, patient, doctor, date and time) before it is committed. A prepared cancellation is valid for 10 minutes and is invalidated early if the appointment changes, or if the permissions or the user change.

## User-visible outcomes

### Outcome: cancelled

The user is told: "Done. Appointment {appointment_reference} for {patient_name} on {appointment_starts_at} is cancelled and its time is free again."

Every value in braces is a fact returned by the application, not wording of the model.
