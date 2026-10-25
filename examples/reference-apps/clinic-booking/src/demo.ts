import { toTimestamp, type Clock } from "./clock.ts";
import { inTransaction, type Database } from "./db.ts";
import { newId } from "./ids.ts";
import { addDays, localDate, zonedToUtc } from "./time.ts";

// A small synthetic demonstration tenant, for running the application by hand.
// It is NOT disposable: test support refuses to touch it. Every name, phone
// number and address is invented (+1-555-01xx, example.test).

export const DEMO_TENANT = "tnt_demo";

const SITES = [
  { id: "site_north", name: "Synthetic Clinic North", timezone: "Europe/Rome" },
  { id: "site_south", name: "Synthetic Clinic South", timezone: "Europe/Rome" },
] as const;

const DOCTORS = ["Dr. Anna Verdi", "Dr. Marco Bianchi"] as const;

const PATIENTS = [
  { given: "Maria", family: "Rossi", dob: "1980-03-14", phone: "+1-555-0101", email: "maria.rossi@example.test" },
  { given: "Luca", family: "Ferri", dob: "1975-11-02", phone: "+1-555-0102", email: "luca.ferri@example.test" },
] as const;

export const seedDemoTenant = (env: { db: Database; clock: Clock }): void => {
  const { db, clock } = env;
  if (db.prepare("SELECT 1 AS found FROM tenants WHERE id = ?").get(DEMO_TENANT) !== undefined) {
    return;
  }
  const now = toTimestamp(clock.now());
  inTransaction(db, () => {
    db.prepare("INSERT INTO tenants (id, name, disposable, created_at) VALUES (?, ?, 0, ?)").run(
      DEMO_TENANT,
      "Synthetic Clinic Group",
      now,
    );
    for (const site of SITES) {
      db.prepare("INSERT INTO sites (tenant_id, id, name, timezone) VALUES (?, ?, ?, ?)").run(
        DEMO_TENANT,
        site.id,
        site.name,
        site.timezone,
      );
      const doctorIds = DOCTORS.map((name) => {
        const id = newId("doc");
        db.prepare("INSERT INTO doctors (tenant_id, site_id, id, name, specialty) VALUES (?, ?, ?, ?, ?)").run(
          DEMO_TENANT,
          site.id,
          id,
          name,
          "general practice",
        );
        return id;
      });
      const patientIds = PATIENTS.map((p) => {
        const id = newId("pat");
        db.prepare(
          `INSERT INTO patients (tenant_id, site_id, id, given_name, family_name, date_of_birth, phone, email, version, created_at, updated_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
        ).run(DEMO_TENANT, site.id, id, p.given, p.family, p.dob, p.phone, p.email, now, now);
        return id;
      });
      const principals: [string, string, string, string | null, string | null][] = [
        [`usr_reception_${site.id}`, `Reception ${site.name}`, "receptionist", null, null],
        [`usr_doctor_${site.id}`, DOCTORS[0], "doctor", null, doctorIds[0] ?? null],
        [`usr_patient_${site.id}`, `${PATIENTS[0].given} ${PATIENTS[0].family}`, "patient", patientIds[0] ?? null, null],
      ];
      for (const [id, name, role, patientId, doctorId] of principals) {
        db.prepare(
          `INSERT INTO principals (tenant_id, site_id, id, display_name, role, patient_id, doctor_id)
           VALUES (?, ?, ?, ?, ?, ?, ?)`,
        ).run(DEMO_TENANT, site.id, id, name, role, patientId, doctorId);
      }
      // Four mornings of half-hour slots starting tomorrow.
      const tomorrow = addDays(localDate(clock.now(), site.timezone), 1);
      for (let day = 0; day < 4; day += 1) {
        const date = addDays(tomorrow, day);
        for (const doctorId of doctorIds) {
          for (const time of ["09:00", "09:30", "10:00", "10:30"]) {
            db.prepare(
              `INSERT INTO slots (tenant_id, site_id, id, doctor_id, starts_at, duration_minutes, status, version, updated_at)
               VALUES (?, ?, ?, ?, ?, 30, 'free', 1, ?)`,
            ).run(
              DEMO_TENANT,
              site.id,
              newId("slot"),
              doctorId,
              toTimestamp(zonedToUtc(date, time, site.timezone)),
              now,
            );
          }
        }
      }
    }
  });
};
