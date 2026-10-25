# List availability

Capability `availability.list`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Lists the free appointment slots of the caller's clinic site, so that one of them can be booked. It changes nothing and needs no confirmation.

Scope: workspace (one clinic site). Roles that may use it: receptionist, doctor, patient.

Operations: `read` is the MCP tool `availability_list`.

## Fields

### Field: date

Type date, optional, default from the context: today. The first day to search, in the clinic site's local time. Defaults to today.

### Field: days

Type integer, optional, from 1 to 14, default 1. How many consecutive days to search, counting from the first day.

### Field: doctor

Type entity_ref of doctor, optional. Only list appointments with this doctor.

A mention is resolved to a doctor record by the application (`resolve_doctor`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: max_results

Type integer, optional, from 1 to 10, default 5. How many free appointments to list at most.

## Validation rules

### Rule: date_in_window

The first day must be today or later and at most 90 days ahead, in the clinic site's local time. A violation is reported as `validation_failed` on date.

### Rule: days_in_range

The number of days must be a whole number from 1 to 14. A violation is reported as `validation_failed` on days.

## Permissions

### Permission: availability.read

Allows listing the free appointments of the caller's clinic site and resolving doctors and slots. Granted to: receptionist, doctor, patient. The host reauthorises every read, validate, prepare, commit and status call from the identity it receives and applies record-level visibility on top of the role, so holding the permission never reaches another clinic site or another clinic group.

## Failure states

### Failure: permission_denied

The principal's role may not list availability.

The user is told: "You are not allowed to look up availability, so I did not search."

### Failure: scope_violation

The doctor named is not a doctor of the principal's clinic site, or does not exist. The application does not say which.

The user is told: "I cannot search for that doctor at your clinic site. Nothing was searched."

### Failure: validation_failed

The date is in the past or too far ahead, or a number is out of range. Nothing was searched.

The user is told: "I could not search: the first day must be from today to 90 days ahead, and the number of days from 1 to 14."

### Failure: unavailable

The clinic database cannot be reached. No result is available and none is guessed.

The user is told: "The appointment system is not reachable right now, so I could not search. Nothing was changed."

## Risk level

### Level 0

Level 0 (read-only, low sensitivity): executed after authorisation, nothing is prepared and nothing is committed. Effect class: none. It only reads. It never books, holds or changes a slot.

## Confirmation rule

### Confirmation: none

No confirmation is asked: the capability only reads. The host still reauthorises the call and limits the answer to the caller's own clinic site.

## User-visible outcomes

### Outcome: slots_found

The user is told: "I found {slot_count} free appointments from {search_from}: {slot_summary}."

Every value in braces is a fact returned by the application, not wording of the model.

### Outcome: none_available

The user is told: "I found no free appointments from {search_from}. That is {slot_count} results."

Every value in braces is a fact returned by the application, not wording of the model.
