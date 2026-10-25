# List work orders

Capability `workorders.list`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

List the work orders of the caller's site that the caller may see, optionally filtered by status, priority, asset or technician, most urgent first. Read-only. A technician sees the work assigned to them and the work they raised; supervisors and auditors see the whole site.

Scope: workspace (one site). Level 0, effect class none.

Operations: `read` is `listWorkOrders`.

## Fields

### Field: status

Type enum (open, assigned, closed, any), optional, default any. Only list work orders in this status; any lists every status.

### Field: priority

Type enum (low, normal, high, urgent), optional. Only list work orders with this priority.

### Field: asset

Type entity_ref of asset, optional. Only list work orders of this asset.

A mention is resolved to a asset record by the application (`resolveAsset`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: technician

Type entity_ref of technician, optional. Only list work orders assigned to this technician. A technician may name only themselves.

A mention is resolved to a technician record by the application (`resolveTechnician`). Several matches are returned as candidates and the user chooses; a record the caller may not see is reported exactly like one that does not exist.

### Field: max_results

Type integer, optional, from 1 to 10, default 5. How many work orders to list at most.

## Validation rules

### Rule: max_results_in_range

The number of work orders must be a whole number from 1 to 10. A violation is reported as `validation_failed` on max_results.

### Rule: technician_filter_scope

A technician may filter by their own name only; another technician's work is not available to them. A violation is reported as `scope_violation` on technician.

## Permissions

### Permission: workorders.read

Allows listing work orders and resolving assets, technicians and work orders. Granted to technician, supervisor, auditor; checked by the host on every call, which also limits a technician to the work assigned to them or raised by them.

## Failure states

### Failure: permission_denied

The principal's role may not use this capability.

The user is told: "You are not allowed to do that, so nothing was changed."

### Failure: scope_violation

A record named is outside the principal's access (another site, another tenant, or work the role may not see), or does not exist. The application does not say which.

The user is told: "I cannot do that: the record is not available to you. Nothing was changed."

### Failure: validation_failed

A value is missing, malformed or out of bounds. Nothing was changed.

The user is told: "I could not list that: ask for 1 to 10 work orders, and for your own work only if you are a technician. Nothing was searched."

### Failure: unavailable

The application's data store cannot be reached. Nothing was changed.

The user is told: "The maintenance system is not reachable right now, so nothing was changed."

## Risk level

### Level 0

Level 0 (read-only, low sensitivity): executed after authorisation; nothing is prepared and nothing is committed. Effect class none.

## Confirmation

### Confirmation: none

No separate confirmation is asked: the application commits right after the concise summary. The host still reauthorises the call and limits it to the caller's own site.

## Outcomes

### Outcome: work_orders_found

What the user is told when the outcome is work_orders_found: I found {work_order_count} work orders (status: {search_status}): {work_order_summary}.

### Outcome: none_found

What the user is told when the outcome is none_found: I found no work orders (status: {search_status}). That is {work_order_count} results.
