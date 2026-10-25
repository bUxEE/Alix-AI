# Decommission an asset

Capability `assets.decommission`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Start the decommissioning of an asset of the caller's site that has no open work orders. This is a level 3 action: the assistant can prepare it and show what it would do, but it can never complete it. The decommissioning is completed only by the supervisor in the Facility Maintenance application's own authenticated interface, after a step-up check, using the reference this capability returns.

Scope: workspace (one site). Level 3, effect class privileged.

Operations: `validate` is `validateDecommissionAsset`, `prepare` is `prepareDecommissionAsset`, `commit` is `commitDecommissionAsset`, `status` is `statusDecommissionAsset`.

## Fields

### Field: asset

Type entity_ref of asset, required. The asset to decommission.

A mention is resolved to a asset record by the application (`resolveAsset`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: reason

Type string, required, length 10 to 300. Why the asset is being retired, for the record.

## Validation rules

### Rule: asset_active

The asset must still be active. A violation is reported as `conflict` on asset.

### Rule: no_open_work_orders

The asset must have no open or assigned work orders. A violation is reported as `conflict` on asset.

### Rule: reason_length

The reason must be 10 to 300 characters of plain text. A violation is reported as `validation_failed` on reason.

## Permissions

### Permission: assets.decommission

Allows starting the decommissioning of the assets of the caller's site. Granted to supervisor only; checked by the host on every call. Completing it needs the supervisor's own step-up in the application's UI, which no role can do through the assistant.

## Failure states

### Failure: permission_denied

The principal's role may not use this capability.

The user is told: "You are not allowed to do that, so nothing was changed."

### Failure: scope_violation

A record named is outside the principal's access (another site, another tenant, or work the role may not see), or does not exist. The application does not say which.

The user is told: "I cannot do that: the record is not available to you. Nothing was changed."

### Failure: validation_failed

A value is missing, malformed or out of bounds. Nothing was changed.

The user is told: "I could not prepare that: give a reason of 10 to 300 characters. Nothing was changed."

### Failure: conflict

A business rule is violated by the current state of the records. Nothing was changed.

The user is told: "I could not prepare that: the asset is already decommissioned or still has open work orders. Nothing was changed."

### Failure: stale

A record changed after the action was prepared, so the preparation is no longer valid. Nothing was changed; it can be prepared again.

The user is told: "A record changed after I prepared this, so I did not apply it. Nothing was changed; I can prepare it again."

### Failure: expired

The prepared action was not confirmed in time, or was withdrawn. Nothing was changed.

The user is told: "This was not confirmed in time, or it was withdrawn, so nothing was changed."

### Failure: step_up_required

The action must be completed in the application's own authenticated interface. The AI path never completes it, and nothing was changed.

The user is told: "This can only be completed in the Facility Maintenance app after a step-up check. Nothing was decommissioned."

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

### Level 3

Level 3 (high-risk, privileged): the assistant can prepare it but never complete it; the supervisor completes it in the application's own authenticated interface after a step-up check. Effect class privileged.

## Confirmation

### Confirmation: step_up

The assistant cannot complete this action. The user completes it in the Facility Maintenance application's own authenticated interface (channel application_ui) after a step-up check; the application's API refuses the commit as step_up_required.

A preparation is honoured for 900 seconds and is invalidated by: record_revision, permission_change, user_change. Commit is idempotent for 86400 seconds: a repeat returns the original receipt.

## Outcomes

### Outcome: decommissioned

What the user is told when the outcome is decommissioned: {asset_name} has been decommissioned in the Facility Maintenance app. The assistant never reports this itself: the supervisor completes it in the app.
