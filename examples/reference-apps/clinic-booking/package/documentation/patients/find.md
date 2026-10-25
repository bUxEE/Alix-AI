# Find a patient

Capability `patients.find`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Finds the patients of the caller's clinic site that the caller is allowed to see, by name and optionally by date of birth. A receptionist sees every patient of the site, a doctor only patients who have an appointment with them, a patient only themselves.

Scope: workspace (one clinic site). Roles that may use it: receptionist, doctor, patient.

Operations: `read` is the MCP tool `patients_find`.

## Fields

### Field: query

Type string, required, length 2 to 100. Part of the patient's name, or the patient's identifier.

### Field: date_of_birth

Type date, optional, sensitive: withheld from logs and from the model unless the task needs it. The patient's date of birth, to narrow the search.

### Field: max_results

Type integer, optional, from 1 to 10, default 5. How many patients to list at most.

## Validation rules

### Rule: query_not_blank

The query must contain at least two characters that are not spaces. A violation is reported as `validation_failed` on query.

### Rule: birth_date_not_future

The date of birth cannot be after today. A violation is reported as `validation_failed` on date_of_birth.

## Permissions

### Permission: patients.read

Allows finding and resolving patients that the caller's role may see (a receptionist the whole site, a doctor their own patients, a patient only themselves). Granted to: receptionist, doctor, patient. The host reauthorises every read, validate, prepare, commit and status call from the identity it receives and applies record-level visibility on top of the role, so holding the permission never reaches another clinic site or another clinic group.

## Failure states

### Failure: permission_denied

The principal's role may not look up patients.

The user is told: "You are not allowed to look up patients, so I did not search."

### Failure: scope_violation

The query is a patient identifier of a record the principal may not see, or one that does not exist. The application does not say which.

The user is told: "I cannot show that patient: the record is not available to you. Nothing was searched."

### Failure: validation_failed

The query is too short or the date of birth is invalid. Nothing was searched.

The user is told: "I could not search: the name needs at least two characters and the date of birth cannot be in the future."

### Failure: missing_fields

A required field (query) was not supplied. Nothing was searched.

The user is told: "I need the name of the patient to search for. Nothing was searched."

### Failure: unavailable

The clinic database cannot be reached. No result is available and none is guessed.

The user is told: "The appointment system is not reachable right now, so I could not search. Nothing was changed."

## Risk level

### Level 0

Level 0 (read-only, low sensitivity): executed after authorisation, nothing is prepared and nothing is committed. Effect class: none. It only reads. It never changes a patient record.

## Confirmation rule

### Confirmation: none

No confirmation is asked: the capability only reads. Which patients appear is decided by the host from the caller's role and clinic site, never by the model.

## User-visible outcomes

### Outcome: patients_found

The user is told: "I found {patient_count} matching patients: {patient_summary}."

Every value in braces is a fact returned by the application, not wording of the model.

### Outcome: none_found

The user is told: "No patient at your clinic site matches {input.query}. That is {patient_count} results: {patient_summary}."

Every value in braces is a fact returned by the application, not wording of the model.
