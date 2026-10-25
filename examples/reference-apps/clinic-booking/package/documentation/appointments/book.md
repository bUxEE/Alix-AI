# Book an appointment

Capability `appointments.book`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Books a free appointment slot of the caller's clinic site for a patient. The booking is prepared first, the exact effects are shown, and it is committed only after the user explicitly confirms. Committing takes the slot for the patient and queues a confirmation message to the patient's contact details.

Scope: workspace (one clinic site). Roles that may use it: receptionist, patient.

Operations: `validate` is the MCP tool `appointments_book_validate`; `prepare` is the MCP tool `appointments_book_prepare`; `commit` is the MCP tool `appointments_book_commit`; `status` is the MCP tool `appointments_book_status`.

A prepared action stays confirmable for 600 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the version of the records the action relies on (the snapshot of those records, not a response tag and not a site-wide counter).

## Fields

### Field: patient

Type entity_ref of patient, required. The patient the appointment is for.

A mention is resolved to a patient record by the application (`resolve_patient`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: slot

Type entity_ref of slot, required. The free appointment slot to book, named by its identifier, by its local date and time, or as the next available one (the application decides which that is).

A mention is resolved to a slot record by the application (`resolve_slot`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: reason

Type string, optional, length 0 to 200, sensitive: withheld from logs and from the model unless the task needs it. The reason for the visit, in the patient's words. Optional free text.

## Validation rules

### Rule: slot_in_window

The slot must start at least 60 minutes from now and at most 90 days ahead. A violation is reported as `validation_failed` on slot.

### Rule: slot_free

The slot must still be free. A slot another booking took is a conflict. A violation is reported as `conflict` on slot.

### Rule: patient_free

The patient must not already have an appointment that overlaps the slot. A violation is reported as `conflict` on patient, slot.

## Permissions

### Permission: appointments.book

Allows booking an appointment for a patient the caller may see, in a slot of the caller's clinic site. Granted to: receptionist, patient. The host reauthorises every read, validate, prepare, commit and status call from the identity it receives and applies record-level visibility on top of the role, so holding the permission never reaches another clinic site or another clinic group.

## Failure states

### Failure: permission_denied

The principal's role may not book appointments.

The user is told: "You are not allowed to book appointments, so nothing was booked."

### Failure: scope_violation

The patient or the slot is outside the principal's access (another site, another clinic group, or a patient the role may not see), or does not exist. The application does not say which.

The user is told: "I cannot book that: the patient or the appointment time is not available to you. Nothing was booked."

### Failure: validation_failed

The slot is too soon or too far ahead, or a value is malformed. Nothing was booked.

The user is told: "I could not book that: an appointment can be booked from 60 minutes to 90 days ahead. Nothing was booked."

### Failure: conflict

The slot is no longer free, or the patient already has an appointment at that time. Nothing was booked.

The user is told: "That appointment time is no longer free, or the patient already has an appointment at that time. Nothing was booked. I can show you other free times."

### Failure: stale

The slot or the patient record changed after the booking was prepared, so the preparation is no longer valid. Nothing was booked; it can be prepared again.

The user is told: "The appointment time or the patient record changed after I prepared this booking, so I did not book it. Nothing was booked; I can prepare it again."

### Failure: expired

The prepared booking was not confirmed in time, or was cancelled. Nothing was booked.

The user is told: "The booking was not confirmed in time, or it was cancelled, so nothing was booked."

### Failure: uncertain_outcome

The commit response was lost. Whether the appointment exists is established through the status operation before anything is retried.

The user is told: "I could not confirm whether the appointment was booked. I will check before doing anything else, so please do not ask for it again yet."

### Failure: missing_fields

A required field (patient or slot) was not supplied. Nothing was booked.

The user is told: "I still need the patient and the appointment time before I can book. Nothing was booked."

### Failure: unavailable

The clinic database cannot be reached. Nothing was booked.

The user is told: "The appointment system is not reachable right now, so nothing was booked."

## Risk level

### Level 2

Level 2 (consequential or external side effect): the action is prepared, its exact effects are shown, and it is committed only after explicit confirmation. Effect class: external. Committing creates an appointment, takes the slot and sends a message to the patient, which is a side effect outside the appointment record. That is why the effect class is external and the level is 2.

## Confirmation rule

### Confirmation: explicit

The user must confirm the exact prepared booking (patient, doctor, date and time, clinic site) before it is committed. A prepared booking is valid for 10 minutes and is invalidated early if the slot or the patient record changes, or if the permissions or the user change.

## User-visible outcomes

### Outcome: booked

The user is told: "Done. {patient_name} is booked with {doctor_name} on {appointment_starts_at} at {site_name}. The appointment reference is {appointment_reference}."

Every value in braces is a fact returned by the application, not wording of the model.
