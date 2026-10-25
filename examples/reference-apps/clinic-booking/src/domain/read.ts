import { patientVisibility, resolveCaller, ROLE_PERMISSIONS, type Caller } from "../access.ts";
import { toTimestamp, type Clock } from "../clock.ts";
import type { Database } from "../db.ts";
import { refuse } from "../errors.ts";
import {
  bool,
  date,
  datetime,
  integer,
  okResult,
  record,
  recordVersion,
  snapshotVersion,
  text,
  type DataVersion,
  type Fact,
  type HostResult,
} from "../results.ts";
import { addDays, daysBetween, localDate, localDisplay, zonedToUtc } from "../time.ts";
import {
  optionalDate,
  optionalInteger,
  optionalReference,
  optionalString,
  parseFields,
  requiredString,
} from "./fields.ts";
import {
  doctorOf,
  getDoctor,
  matchesName,
  normalise,
  patientName,
  type AppointmentRow,
  type DoctorRow,
  type PatientRow,
  type SlotRow,
} from "./records.ts";
import { MAX_AHEAD_DAYS, MIN_LEAD_MINUTES } from "./rules.ts";

const PATIENT_ID_PATTERN = /^pat_[0-9a-z]{6,40}$/u;
const DOCTOR_ID_PATTERN = /^doc_[0-9a-z]{6,40}$/u;
const SLOT_ID_PATTERN = /^slot_[0-9a-z]{6,40}$/u;
const APPOINTMENT_ID_PATTERN = /^apt_[0-9a-z]{6,40}$/u;
const REFERENCE_PATTERN = /^APT-[0-9A-Z]{6}$/iu;
const SLOT_MENTION = /^(\d{4}-\d{2}-\d{2})(?:[ T](\d{2}:\d{2}))?$/u;

const freshnessOf = (clock: Clock, versions: DataVersion[]) => ({
  as_of: toTimestamp(clock.now()),
  version: snapshotVersion(versions),
});

const outOfScope = (field: string) =>
  refuse("scope_violation", "the record is outside the caller's access scope or does not exist", { field });

// ---- availability.list -----------------------------------------------------

export const availabilityList = (db: Database, clock: Clock, identity: unknown, rawFields: unknown): HostResult => {
  const caller = resolveCaller(db, identity, "availability.read");
  const fields = parseFields(rawFields, ["date", "days", "doctor", "max_results"]);
  const now = clock.now();
  const today = localDate(now, caller.timezone);
  const firstDate = optionalDate(fields, "date") ?? today;
  if (firstDate < today || daysBetween(today, firstDate) > MAX_AHEAD_DAYS) {
    throw refuse("validation_failed", `date must be between today and ${MAX_AHEAD_DAYS} days ahead`, {
      field: "date",
    });
  }
  const days = optionalInteger(fields, "days", { min: 1, max: 14, fallback: 1 });
  const maxResults = optionalInteger(fields, "max_results", { min: 1, max: 10, fallback: 5 });
  const doctorRef = optionalReference(fields, "doctor");
  const doctor = doctorRef === undefined ? undefined : getDoctor(db, caller, doctorRef);

  const earliest = new Date(now.getTime() + MIN_LEAD_MINUTES * 60_000);
  const windowStart = zonedToUtc(firstDate, "00:00", caller.timezone);
  const from = windowStart > earliest ? windowStart : earliest;
  const to = zonedToUtc(addDays(firstDate, days), "00:00", caller.timezone);

  const params: (string | number)[] = [caller.tenantId, caller.siteId, toTimestamp(from), toTimestamp(to)];
  let doctorClause = "";
  if (doctor !== undefined) {
    doctorClause = "AND s.doctor_id = ?";
    params.push(doctor.id);
  }
  params.push(maxResults + 1);
  const rows = db
    .prepare(
      `SELECT s.*, d.name AS doctor_name FROM slots s
         JOIN doctors d ON d.tenant_id = s.tenant_id AND d.site_id = s.site_id AND d.id = s.doctor_id
        WHERE s.tenant_id = ? AND s.site_id = ? AND s.status = 'free' AND s.starts_at >= ? AND s.starts_at < ?
              ${doctorClause}
        ORDER BY s.starts_at, s.id LIMIT ?`,
    )
    .all(...params) as (SlotRow & { doctor_name: string })[];
  const moreAvailable = rows.length > maxResults;
  const listed = rows.slice(0, maxResults);

  const values: Fact[] = [
    date("search_from", firstDate),
    integer("slot_count", listed.length),
    bool("more_available", moreAvailable),
    text(
      "slot_summary",
      listed.length === 0
        ? "no free appointments"
        : listed.map((s) => `${localDisplay(new Date(s.starts_at), caller.timezone)} ${s.doctor_name}`).join("; "),
    ),
  ];
  listed.forEach((slot, index) => {
    const n = index + 1;
    values.push(
      datetime(
        `slot_${n}_starts_at`,
        slot.starts_at,
        caller.timezone,
        localDisplay(new Date(slot.starts_at), caller.timezone),
      ),
      text(`slot_${n}_doctor`, slot.doctor_name),
    );
  });
  return okResult({
    identifiers: listed.map((s) => record("slot", s.id)),
    values,
    freshness: freshnessOf(
      clock,
      listed.map((s) => recordVersion("slot", s.id, s.version)),
    ),
    allowed_next_actions:
      listed.length > 0 && ROLE_PERMISSIONS[caller.role].includes("appointments.book")
        ? [{ action: "invoke_capability", capability_id: "appointments.book", label: "Book one of these appointments" }]
        : [],
  });
};

// ---- patients.find ---------------------------------------------------------

const patientBirthSummary = (patient: PatientRow): string => `${patientName(patient)} (born ${patient.date_of_birth})`;

const visiblePatients = (db: Database, caller: Caller): PatientRow[] => {
  const visible = patientVisibility(caller);
  return db
    .prepare(
      `SELECT p.* FROM patients p WHERE p.tenant_id = ? AND p.site_id = ? AND (${visible.sql})
        ORDER BY p.family_name, p.given_name, p.id`,
    )
    .all(caller.tenantId, caller.siteId, ...visible.params) as PatientRow[];
};

export const patientsFind = (db: Database, clock: Clock, identity: unknown, rawFields: unknown): HostResult => {
  const caller = resolveCaller(db, identity, "patients.read");
  const fields = parseFields(rawFields, ["query", "date_of_birth", "max_results"]);
  const query = requiredString(fields, "query", { min: 2, max: 100 });
  const dateOfBirth = optionalDate(fields, "date_of_birth");
  if (dateOfBirth !== undefined && dateOfBirth > localDate(clock.now(), caller.timezone)) {
    throw refuse("validation_failed", "date_of_birth cannot be in the future", { field: "date_of_birth" });
  }
  const maxResults = optionalInteger(fields, "max_results", { min: 1, max: 10, fallback: 5 });

  let matches: PatientRow[];
  if (PATIENT_ID_PATTERN.test(query)) {
    // An identifier is an address, not a search: it either names a patient the
    // caller may see or it is refused like any other out-of-scope record.
    const found = visiblePatients(db, caller).find((p) => p.id === query);
    if (found === undefined) {
      throw outOfScope("query");
    }
    matches = [found];
  } else {
    matches = visiblePatients(db, caller).filter((p) => matchesName(query, patientName(p)));
  }
  if (dateOfBirth !== undefined) {
    matches = matches.filter((p) => p.date_of_birth === dateOfBirth);
  }
  const listed = matches.slice(0, maxResults);

  const values: Fact[] = [
    integer("patient_count", listed.length),
    bool("more_available", matches.length > listed.length),
    text("patient_summary", listed.length === 0 ? "no matching patients" : listed.map(patientBirthSummary).join("; ")),
  ];
  listed.forEach((patient, index) => {
    const n = index + 1;
    values.push(
      text(`patient_${n}_name`, patientName(patient)),
      date(`patient_${n}_date_of_birth`, patient.date_of_birth),
    );
  });
  const offers = [
    { capability: "appointments.book", permission: "appointments.book", label: "Book an appointment for one of these patients" },
    {
      capability: "patients.update_contact",
      permission: "patients.update_contact",
      label: "Update the contact details of one of these patients",
    },
  ] as const;
  return okResult({
    identifiers: listed.map((p) => record("patient", p.id)),
    values,
    freshness: freshnessOf(
      clock,
      listed.map((p) => recordVersion("patient", p.id, p.version)),
    ),
    allowed_next_actions:
      listed.length === 0
        ? []
        : offers
            .filter((offer) => ROLE_PERMISSIONS[caller.role].includes(offer.permission))
            .map((offer) => ({
              action: "invoke_capability" as const,
              capability_id: offer.capability,
              label: offer.label,
            })),
  });
};

// ---- entity resolvers ------------------------------------------------------
//
// The core resolves a mention ("Dr Verdi", "2026-10-14 10:30") to candidate
// identifiers before it prepares anything, and asks the user when there are
// several. It does it with an adapter READ that carries the reserved arguments
// entity_type, mention and, when the model proposed one, candidate_id; the MCP
// adapter maps that read to the resolve tool the package declares for the entity
// type (entity_types.<type>.resolve).
//
// A resolver is a read and decides nothing. It answers with the candidates the
// caller may see, in a stable order, as `identifiers` (record references of the
// entity type) plus one text value named `label` for each, at the same position,
// and the snapshot version of those records as freshness. Several matches are
// several candidates: the core asks the user which one. A mention that matches
// nothing is an empty list; a record the caller may not see is refused as
// scope_violation, exactly like one that does not exist.
//
// candidate_id is the model's proposal, so it is never an instruction: the core
// forwards whatever identifier the model proposed, and the user's own choice
// between candidates travels as a structured choice, not as a hint. With no
// mention the hint names the one record to re-resolve and is read like an
// identifier mention (access-checked, refused like an unknown record when the
// caller may not see it). With a mention the host resolves the mention and the
// hint changes nothing: it never narrows several matches to one (that would let
// the model pick which Maria Rossi is booked), never adds a candidate and is not
// even looked up.

type Candidate = { id: string; version: number; label: string };

const MAX_CANDIDATES = 10;

const candidateResult = (clock: Clock, entityType: string, candidates: Candidate[]): HostResult =>
  okResult({
    identifiers: candidates.map((c) => record(entityType, c.id)),
    values: [integer("candidate_count", candidates.length), ...candidates.map((c) => text("label", c.label))],
    freshness: freshnessOf(
      clock,
      candidates.map((c) => recordVersion(entityType, c.id, c.version)),
    ),
  });

// The reserved arguments of a resolution read. entity_type is optional on the
// wire (a direct call may omit it) but, when present, must be this resolver's
// own type: a resolver never answers for another entity type.
const resolverArguments = (rawFields: unknown, entityType: string): { mention: string } => {
  const fields = parseFields(rawFields, ["entity_type", "mention", "candidate_id"]);
  const stated = optionalString(fields, "entity_type", { max: 64 });
  if (stated !== undefined && stated !== entityType) {
    throw refuse("validation_failed", `this resolver answers for ${entityType}, not ${stated}`, { field: "entity_type" });
  }
  const candidateId = optionalReference(fields, "candidate_id");
  const mention = optionalString(fields, "mention", { max: 100 });
  if (mention === undefined) {
    if (candidateId === undefined) {
      throw refuse("missing_fields", "mention is required", { field: "mention" });
    }
    return { mention: candidateId };
  }
  // The hint was checked for its shape above and is deliberately not used any further.
  return { mention };
};

// "the next available appointment" and "il primo orario libero": the host, not the model,
// decides which slot that is (the earliest one that can still be booked at the caller's
// site), so the user can ask for it without naming a date. It needs an ordinal word and a
// word for the thing, and anything else is an ordinary mention.
const NEXT_WORDS = new Set(["next", "earliest", "soonest", "first", "prossimo", "prossima", "primo", "prima"]);
const NEXT_THINGS = new Set(["available", "free", "appointment", "slot", "one", "opening", "disponibile", "libero", "libera", "orario", "appuntamento", "visita"]);

const asksForNextAvailable = (mention: string): boolean => {
  const words = normalise(mention)
    .replace(/[^a-z0-9 ]+/gu, " ")
    .split(/\s+/u)
    .filter((word) => word !== "");
  return words.some((w) => NEXT_WORDS.has(w)) && words.some((w) => NEXT_THINGS.has(w));
};

const SELF_WORDS = new Set(["me", "myself", "i", "my", "io", "mio", "mia"]);

export const resolvePatient = (db: Database, clock: Clock, identity: unknown, rawFields: unknown): HostResult => {
  const caller = resolveCaller(db, identity, "patients.read");
  const { mention } = resolverArguments(rawFields, "patient");
  const visible = visiblePatients(db, caller);
  let found: PatientRow[];
  if (PATIENT_ID_PATTERN.test(mention)) {
    found = visible.filter((p) => p.id === mention);
    if (found.length === 0) {
      throw outOfScope("mention");
    }
  } else if (caller.role === "patient" && SELF_WORDS.has(normalise(mention))) {
    found = visible;
  } else {
    found = visible.filter((p) => matchesName(mention, patientName(p)));
  }
  return candidateResult(
    clock,
    "patient",
    found.slice(0, MAX_CANDIDATES).map((p) => ({ id: p.id, version: p.version, label: patientBirthSummary(p) })),
  );
};

export const resolveDoctor = (db: Database, clock: Clock, identity: unknown, rawFields: unknown): HostResult => {
  const caller = resolveCaller(db, identity, "availability.read");
  const { mention } = resolverArguments(rawFields, "doctor");
  const doctors = db
    .prepare("SELECT * FROM doctors WHERE tenant_id = ? AND site_id = ? ORDER BY name, id")
    .all(caller.tenantId, caller.siteId) as DoctorRow[];
  let found: DoctorRow[];
  if (DOCTOR_ID_PATTERN.test(mention)) {
    found = doctors.filter((d) => d.id === mention);
    if (found.length === 0) {
      throw outOfScope("mention");
    }
  } else {
    // "Dr", "Dr." and "doctor" are titles, not part of a name.
    const cleaned = mention.replace(/\b(dr|dott|dottor|dottoressa|doctor)\b\.?/giu, " ").trim();
    found = cleaned === "" ? [] : doctors.filter((d) => matchesName(cleaned, d.name));
  }
  // A doctor row has no revision of its own: it never changes in this
  // application, so its version is the constant 1.
  return candidateResult(
    clock,
    "doctor",
    found.slice(0, MAX_CANDIDATES).map((d) => ({ id: d.id, version: 1, label: `${d.name} (${d.specialty})` })),
  );
};

export const resolveSlot = (db: Database, clock: Clock, identity: unknown, rawFields: unknown): HostResult => {
  const caller = resolveCaller(db, identity, "availability.read");
  const { mention } = resolverArguments(rawFields, "slot");
  const slots = db
    .prepare(
      `SELECT s.* FROM slots s WHERE s.tenant_id = ? AND s.site_id = ? AND s.status = 'free' AND s.starts_at > ?
        ORDER BY s.starts_at, s.id`,
    )
    .all(caller.tenantId, caller.siteId, toTimestamp(clock.now())) as SlotRow[];
  let found: SlotRow[];
  const byTime = SLOT_MENTION.exec(mention);
  if (SLOT_ID_PATTERN.test(mention)) {
    found = slots.filter((s) => s.id === mention);
    if (found.length === 0) {
      // A slot of this site that is no longer free is not a candidate; one
      // that is not in this site is out of scope, whether or not it exists.
      const exists = db
        .prepare("SELECT 1 AS found FROM slots WHERE tenant_id = ? AND site_id = ? AND id = ?")
        .get(caller.tenantId, caller.siteId, mention);
      if (exists === undefined) {
        throw outOfScope("mention");
      }
    }
  } else if (byTime !== null) {
    const [, day = "", time] = byTime;
    found = slots.filter((s) => {
      const shown = localDisplay(new Date(s.starts_at), caller.timezone);
      return time === undefined ? shown.startsWith(day) : shown === `${day} ${time}`;
    });
  } else if (asksForNextAvailable(mention)) {
    const bookableFrom = toTimestamp(new Date(clock.now().getTime() + MIN_LEAD_MINUTES * 60_000));
    found = slots.filter((s) => s.starts_at >= bookableFrom).slice(0, 1);
  } else {
    throw refuse("validation_failed", "a slot is named by its identifier, by YYYY-MM-DD HH:MM or as the next available one", {
      field: "mention",
    });
  }
  return candidateResult(
    clock,
    "slot",
    found.slice(0, MAX_CANDIDATES).map((s) => ({
      id: s.id,
      version: s.version,
      label: `${localDisplay(new Date(s.starts_at), caller.timezone)} with ${doctorOf(db, caller.tenantId, caller.siteId, s.doctor_id).name}`,
    })),
  );
};

export const resolveAppointment = (db: Database, clock: Clock, identity: unknown, rawFields: unknown): HostResult => {
  const caller = resolveCaller(db, identity, "appointments.cancel");
  const { mention } = resolverArguments(rawFields, "appointment");
  const rows = db
    .prepare(
      `SELECT a.*, p.given_name, p.family_name FROM appointments a
         JOIN patients p ON p.tenant_id = a.tenant_id AND p.site_id = a.site_id AND p.id = a.patient_id
        WHERE a.tenant_id = ? AND a.site_id = ? AND a.status = 'booked' AND a.starts_at > ?
        ORDER BY a.starts_at, a.id`,
    )
    .all(caller.tenantId, caller.siteId, toTimestamp(clock.now())) as (AppointmentRow & {
    given_name: string;
    family_name: string;
  })[];
  // The same visibility as everywhere else: a patient sees their own
  // appointments, a doctor theirs, a receptionist the site's.
  const visible = rows.filter((a) => {
    switch (caller.role) {
      case "receptionist":
        return true;
      case "doctor":
        return a.doctor_id === caller.doctorId;
      case "patient":
        return a.patient_id === caller.patientId;
    }
  });
  let found: typeof visible;
  const byTime = SLOT_MENTION.exec(mention);
  if (APPOINTMENT_ID_PATTERN.test(mention)) {
    found = visible.filter((a) => a.id === mention);
    if (found.length === 0) {
      throw outOfScope("mention");
    }
  } else if (REFERENCE_PATTERN.test(mention)) {
    found = visible.filter((a) => a.reference === mention.toUpperCase());
  } else if (byTime !== null) {
    const [, day = "", time] = byTime;
    found = visible.filter((a) => {
      const shown = localDisplay(new Date(a.starts_at), caller.timezone);
      return time === undefined ? shown.startsWith(day) : shown === `${day} ${time}`;
    });
  } else {
    found = visible.filter((a) => matchesName(mention, `${a.given_name} ${a.family_name}`));
  }
  return candidateResult(
    clock,
    "appointment",
    found.slice(0, MAX_CANDIDATES).map((a) => ({
      id: a.id,
      version: a.version,
      label: `${a.reference}: ${a.given_name} ${a.family_name}, ${localDisplay(new Date(a.starts_at), caller.timezone)} with ${doctorOf(db, caller.tenantId, caller.siteId, a.doctor_id).name}`,
    })),
  );
};
