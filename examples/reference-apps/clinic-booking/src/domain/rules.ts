// Business limits of the clinic, in one place so the validation rules, the
// listing and the documentation cannot drift apart.

// A slot must start at least this long from now to be bookable.
export const MIN_LEAD_MINUTES = 60;

// And no further ahead than this.
export const MAX_AHEAD_DAYS = 90;

// How long a prepared action stays confirmable (the package declares the same).
export const BOOKING_TTL_SECONDS = 600;
export const CANCEL_TTL_SECONDS = 600;
export const CONTACT_TTL_SECONDS = 120;

// How long the host remembers an idempotency key.
export const IDEMPOTENCY_WINDOW_SECONDS = 86_400;

export const CANCEL_REASONS = ["patient_request", "provider_unavailable", "scheduling_error", "other"] as const;
export type CancelReason = (typeof CANCEL_REASONS)[number];

export const MAX_BODY_BYTES = 1 << 20;
