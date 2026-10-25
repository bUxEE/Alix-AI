// Package settings is the settings and client preferences API of AlixAI (master
// plan 14.2, "Settings and client preferences"): typed, versioned and
// permission-checked settings at user, tenant and deployment level, the approved
// model and resource profiles, the capabilities enabled for the caller, the
// integration-audit findings and the provider health. The HTTP surface is
// internal/gateway/routes_v1_settings.go and the contract is
// schemas/api/paths/settings.yaml over the documents in schemas/settings.
//
// The last paragraph of the master plan section is the security requirement: "A
// client cannot use settings to enable an undeclared tool, enlarge a scope, bypass
// confirmation or expose LocalAI administration." The external-provider and
// decision-layer plans add: settings can not enable external or hybrid mode,
// change data-egress policy, hold provider credentials, raise a budget or change
// the decision-layer configuration. Those belong to configuration and to
// operator-only APIs of later milestones.
//
// # How the guarantee is built
//
// It is not a list of special cases in a validator; it is the shape of the
// contract. The three document schemas are closed (additionalProperties false at
// every level) and have no member for any of those things, so a document that
// names one is an unknown member and is refused whole (validation_failed, with
// details.guard naming the rule the member runs into, nothing stored, nothing
// recorded). What a setting CAN say is limited to three kinds of statement:
//
//   - select among things the operator or the package defined: a model profile
//     (the settings.approved_profiles configuration), a voice (approved_voices), a
//     language (the active package's supported languages), a capability (one the
//     policy gateway lets the caller use);
//   - switch something off for oneself or one's tenant: a capability, voice,
//     attachments, history export, notifications, the transcript;
//   - ask for MORE: confirmation from a risk level up (the platform floor, level 2
//     explicit confirmation and level 3 step-up, is not a setting and can not be
//     lowered), a shorter retention, a smaller attachment size.
//
// Every string a document can hold is therefore an element of a closed set or an
// enum, which is also why no secret can be stored in one. Settings are never an
// input to the policy gateway: what a model may use is decided from the package,
// the token and the host policy alone, and the tests compare the gateway's
// decisions (exposure and commit evidence) before and after the strongest valid
// use of settings. The effective view and the capability view only read.
//
// # Levels and keys
//
//   - user       the token's principal in the token's workspace
//   - tenant     the token's tenant of the token's application
//   - deployment the token's application as a whole
//
// "Deployment" is per application, never installation-wide (ADR 0016: no
// application sees another's scope, keys or records). What the operator controls
// for the whole installation is the settings configuration section, not a setting.
// The key of a document comes from the verified Scope and from nothing else (the
// store helpers that bind the scope columns stop at the workspace, so this module
// builds its own predicate, from a Validate()d scope only, and the table's CHECK
// refuses any row whose finer scope columns are not empty at the coarser levels).
//
// # Permission
//
// A token must carry the capability scope for the level and the operation:
// settings.user.read, settings.user.write, settings.tenant.read and write,
// settings.deployment.read and write, an exact match (a wildcard makes the token
// itself malformed). The host chooses which of its principals get which by putting
// them in their tokens. The integration-audit findings and the provider health
// need settings.deployment.read; the effective view, the profiles and the
// capability view need settings.user.read. An application whose package is not
// active serves no settings (quarantined), except the integration-audit read-out,
// which matters most while an application is quarantined.
//
// # Typed and versioned
//
// A document is validated against the published schema of its level (strict JSON
// first: one object, no duplicate member, at most 16 KiB and six levels deep), then
// against the rules a schema can not state (approved profile and voice, supported
// language, the operator's ceilings, every named capability being one the caller
// may use), decoded into the generated type (pkg/contracts/gen/settings) and
// stored in canonical form with the urn of its schema. Versioned means two things:
// the schema carries its version (v1), so a stored document can be migrated rather
// than guessed at (a stored document of a schema this build does not know fails
// closed), and every document has a revision that rises by exactly one per change.
//
// # Optimistic concurrency
//
// A write carries the revision the caller read (0 to create) and succeeds only
// against it; the loser gets conflict with the current version and nothing
// changes. The revision check, the write and the audit event are one store
// transaction. A document identical to the stored one changes nothing and records
// nothing.
//
// # Audit
//
// Every change is the typed audit event settings.document_written (registered
// from this package through the audit registry): level, new and previous revision,
// schema, the digest of the new canonical document and the names of the sections
// that changed, never a value. It goes to the tenant chain for user and tenant
// settings and to the application chain for deployment settings, in the same
// transaction as the change: a change that can not be recorded does not happen.
//
// # The cascade
//
// Effective combines the levels with fixed rules, never a model. Preferences take
// the most specific level that states them. Voice, attachments and history export
// are off until the deployment level allows them and each level below can only keep
// or switch them off; retention and attachment size are the smallest value any
// level states, capped by the operator's ceiling; switched-off capabilities add up
// across levels; extra confirmation takes the lowest threshold, so a user can never
// relax what a tenant asked for. decided_by names which level decided each value.
//
// # What it does not do
//
// It does not apply a profile to inference (the dialogue engine honours the
// effective profile within the approved set), enforce retention (internal/history,
// M3-08) or run an audit, accept a warning or activate a package
// (internal/integrationaudit): the read-outs are read-only.
package settings
