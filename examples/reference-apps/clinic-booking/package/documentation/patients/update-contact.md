# Update contact details

Capability `patients.update_contact`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Changes the phone number, the email address or both on a patient's record. A patient may change their own record, a receptionist any record of the site. The change is reversible and low-impact, so the application allows it to commit right after it has been prepared and summarised.

Scope: workspace (one clinic site). Roles that may use it: receptionist, patient.

Operations: `validate` is the MCP tool `patients_update_contact_validate`; `prepare` is the MCP tool `patients_update_contact_prepare`; `commit` is the MCP tool `patients_update_contact_commit`; `status` is the MCP tool `patients_update_contact_status`.

A prepared action stays confirmable for 120 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the version of the records the action relies on (the snapshot of those records, not a response tag and not a site-wide counter).

## Fields

### Field: patient

Type entity_ref of patient, required. The patient whose contact details change.

A mention is resolved to a patient record by the application (`resolve_patient`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: phone

Type string, optional, length 5 to 32, sensitive: withheld from logs and from the model unless the task needs it. The new phone number.

### Field: email

Type string, optional, length 0 to 254, sensitive: withheld from logs and from the model unless the task needs it. The new email address.

## Validation rules

### Rule: contact_change_present

At least one of the phone number and the email address must be given. A violation is reported as `validation_failed` on phone, email.

### Rule: phone_format

A phone number holds at least 5 digits and only digits, spaces and + - ( ) . characters. A violation is reported as `validation_failed` on phone.

### Rule: email_format

An email address has the form name@domain.tld and at most 254 characters. A violation is reported as `validation_failed` on email.

## Permissions

### Permission: patients.update_contact

Allows changing the phone number and the email address on a patient record the caller may change. Granted to: receptionist, patient. The host reauthorises every read, validate, prepare, commit and status call from the identity it receives and applies record-level visibility on top of the role, so holding the permission never reaches another clinic site or another clinic group.

## Failure states

### Failure: permission_denied

The principal's role may not change contact details.

The user is told: "You are not allowed to change contact details, so nothing was updated."

### Failure: scope_violation

The patient record is outside the principal's access (another site, another clinic group, or another patient for a patient), or does not exist. The application does not say which.

The user is told: "I cannot change that record: it is not available to you. Nothing was updated."

### Failure: validation_failed

Nothing to change was given, or the phone number or email address is malformed. Nothing was updated.

The user is told: "I could not update that: give a new phone number, a new email address or both, and check their format. Nothing was updated."

### Failure: stale

The patient record changed after the update was prepared, so the preparation is no longer valid. Nothing was updated; it can be prepared again.

The user is told: "The patient record changed after I prepared this update, so I did not apply it. Nothing was updated; I can prepare it again."

### Failure: expired

The prepared update was not committed in time, or was withdrawn. Nothing was updated.

The user is told: "The update was not applied in time, or it was withdrawn, so nothing was updated."

### Failure: uncertain_outcome

The commit response was lost. Whether the details were updated is established through the status operation before anything is retried.

The user is told: "I could not confirm whether the contact details were updated. I will check before doing anything else, so please do not ask for it again yet."

### Failure: missing_fields

A required field (patient) was not supplied. Nothing was updated.

The user is told: "I still need to know whose contact details to change. Nothing was updated."

### Failure: unavailable

The clinic database cannot be reached. Nothing was updated.

The user is told: "The appointment system is not reachable right now, so nothing was updated."

## Risk level

### Level 1

Level 1 (reversible, low-impact change): a concise summary is shown and the application allows the change to commit immediately. Effect class: reversible. Committing updates the contact details on one patient record. Nothing leaves the application and the change can be undone by changing the details back, so the effect class is reversible and the level is 1.

## Confirmation rule

### Confirmation: none

No separate confirmation is asked (confirmation mode none): the application allows a level 1 change to commit immediately after a concise summary. A prepared update is valid for 2 minutes and is invalidated early if the patient record changes.

## User-visible outcomes

### Outcome: contact_updated

The user is told: "Done. The contact details on file for {patient_name} were updated: {contact_fields_updated}."

Every value in braces is a fact returned by the application, not wording of the model.
