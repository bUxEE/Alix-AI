package apppkg

import (
	"regexp"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// mutation is one defect built into a copy of the valid package, and the
// checks it must raise. The package is re-signed after the edit, so the
// signature is never the reason for a finding.
type mutation struct {
	name string
	edit func(t *testing.T, p pkgFiles)
	// crit and warn are check IDs. Unless loose, the critical findings must be
	// exactly crit, which proves the defect is the only one present; loose
	// lists the checks that must be present among others the defect causes.
	crit  []string
	warn  []string
	loose bool
	// unsigned loads the package without a signature: for a defect that keeps
	// the package from being signed at all.
	unsigned bool
}

const (
	capReserve = "capabilities/reservations-create.yaml"
	capFind    = "capabilities/catalog-find-title.yaml"
	ctrRsvEN   = "conversation-contracts/reservations-create.en.yaml"
	ctrRsvIT   = "conversation-contracts/reservations-create.it.yaml"
	docIndex   = "documentation/index.yaml"
	policyFile = "policies/default-access.yaml"
	termFile   = "terminology/library-terms.yaml"
	srcFile    = "rag-sources/loan-policies.yaml"
	testsFile  = "tests/core-safety.yaml"
)

var mutations = []mutation{
	// ---- package layout and authoring rules (14.1) ----
	{name: "unexpected file at the root", edit: func(t *testing.T, p pkgFiles) { p.set("NOTES.txt", "hello\n") }, warn: []string{"package.unexpected_file"}},
	{name: "unexpected directory", edit: func(t *testing.T, p pkgFiles) { p.set("extras/readme.txt", "hello\n") }, warn: []string{"package.unexpected_file"}},
	{name: "terminology file in a subdirectory", edit: func(t *testing.T, p pkgFiles) { p.set("terminology/sub/x.yaml", "terms: []\n") }, warn: []string{"package.unexpected_file"}},
	{name: "no manifest", edit: func(t *testing.T, p pkgFiles) { p.remove(t, "alixai.yaml") }, crit: []string{"manifest.missing"}, unsigned: true},
	{name: "manifest does not match its schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, "alixai.yaml", "name: Demo Library Desk") }, crit: []string{"schema.manifest"}},
	{name: "capability does not match its schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, capReserve, "risk_level: 2") }, crit: []string{"schema.capability"}, loose: true},
	{name: "contract does not match its schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, ctrRsvEN, "contract_version: 1.0.0") }, crit: []string{"schema.conversation_contract", "contract.missing"}},
	{name: "documentation index does not match its schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, docIndex, "title: 'Field: query'") }, crit: []string{"schema.documentation_index"}, loose: true},
	{name: "policy does not match its schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, policyFile, "policy_id: default-access") }, crit: []string{"schema.policy"}, loose: true},
	{name: "terminology does not match its schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, termFile, "preferred: title") }, crit: []string{"schema.terminology"}, loose: true},
	{name: "tests do not match their schema", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, testsFile, "synthetic: true") }, crit: []string{"schema.package_tests"}, loose: true},
	{name: "unknown property is an error", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capFind, "audience: user", "audience: user\nsurprise: true")
	}, crit: []string{"schema.capability"}, loose: true},

	// ---- manifest consistency (14.5, 14.6) ----
	{name: "default language not supported", edit: func(t *testing.T, p pkgFiles) { p.edit(t, "alixai.yaml", "  default: en", "  default: fr") }, crit: []string{"manifest.language_default_unsupported"}, loose: true},
	{name: "listed capability file missing", edit: func(t *testing.T, p pkgFiles) { p.remove(t, capFind) }, crit: []string{"manifest.capability_file_missing"}, loose: true},
	{name: "capability id differs from the manifest key", edit: func(t *testing.T, p pkgFiles) { p.edit(t, capFind, "id: catalog.find_title", "id: catalog.find_other") }, crit: []string{"manifest.capability_id_mismatch"}, loose: true},
	{name: "capability file not listed", edit: func(t *testing.T, p pkgFiles) { p.set("capabilities/unlisted.yaml", string(p[capFind])) }, crit: []string{"manifest.capability_file_unlisted"}},
	{name: "listed source file missing", edit: func(t *testing.T, p pkgFiles) { p.remove(t, srcFile) }, crit: []string{"manifest.rag_source_file_missing"}, loose: true},
	{name: "source id differs from the manifest key", edit: func(t *testing.T, p pkgFiles) { p.edit(t, srcFile, "id: loan-policies", "id: loan-rules") }, crit: []string{"manifest.rag_source_id_mismatch"}, loose: true},
	{name: "source file not listed", edit: func(t *testing.T, p pkgFiles) { p.set("rag-sources/unlisted.yaml", string(p[srcFile])) }, crit: []string{"manifest.rag_source_file_unlisted"}},
	{name: "adapter config missing", edit: func(t *testing.T, p pkgFiles) { p.remove(t, "schemas/adapters/library-mcp.yaml") }, crit: []string{"endpoint.file_missing"}},
	{name: "endpoint host without egress entry", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, "alixai.yaml", "https://library.example.test/mcp", "https://elsewhere.example.test/mcp")
	}, crit: []string{"endpoint.egress_missing"}},
	{name: "result schema missing", edit: func(t *testing.T, p pkgFiles) { p.remove(t, "schemas/find-title.result.schema.json") }, crit: []string{"capability.result_schema_missing"}},

	// ---- adapter mapping, SEC-052 ----
	{name: "look-alike operation names", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "mcp_tool: validate_reservation", "mcp_tool: Prepare-Reservation")
	}, crit: []string{"adapter.lookalike_operation"}},
	{name: "one operation mapped by two capabilities", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capFind, "mcp_tool: find_title", "mcp_tool: prepare_reservation")
	}, warn: []string{"adapter.operation_shared"}},

	// ---- capability fields (14.7 to 14.9) ----
	{name: "entity_ref names an undeclared entity type", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "  entity_type: title\n  required: true", "  entity_type: book\n  required: true")
	}, crit: []string{"capability.entity_type_unknown"}, loose: true},
	{name: "default of the wrong type", edit: func(t *testing.T, p pkgFiles) { p.edit(t, capFind, "    value: 5", "    value: five") }, crit: []string{"capability.default_type_mismatch"}},
	{name: "constraint on the wrong type", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capFind, "  minimum: 1\n  maximum: 20", "  minimum: 1\n  maximum: 20\n  min_length: 3")
	}, crit: []string{"capability.constraint_misapplied"}, loose: true},
	{name: "ambiguity rule on an undeclared field", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "- id: title_multiple\n  field: title", "- id: title_multiple\n  field: titel")
	}, crit: []string{"capability.ambiguity_invalid"}, loose: true},
	{name: "example expects an undeclared field", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "    title: The Leopard", "    titel: The Leopard")
	}, crit: []string{"capability.example_invalid"}},
	{name: "example in an unsupported language", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "  language: it\n  test_only: true", "  language: fr\n  test_only: true")
	}, warn: []string{"capability.example_language_unsupported"}},
	{name: "next action to an undeclared capability", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "  capability_id: catalog.find_title", "  capability_id: nowhere.to_go")
	}, crit: []string{"capability.next_action_invalid"}, loose: true},
	{name: "validation rule on an undeclared field", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "- id: title_available\n  fields:\n  - title", "- id: title_available\n  fields:\n  - titel")
	}, crit: []string{"capability.validation_invalid"}},
	{name: "validation rule with an error code the capability does not declare", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capFind, "  description: The query must contain at least two characters that are not spaces.\n  error_code: validation_failed", "  description: The query must contain at least two characters that are not spaces.\n  error_code: conflict")
	}, crit: []string{"capability.validation_invalid"}},
	{name: "idempotency window shorter than the preparation", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "  window_seconds: 86400", "  window_seconds: 60")
	}, crit: []string{"capability.idempotency_window"}},
	{name: "duplicate example id", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "- id: en-reserve-with-note", "- id: en-reserve-for-friday")
	}, crit: []string{"unique.duplicate_id"}},

	// ---- documentation (14.10) ----
	{name: "doc_refs names an unknown entry", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "doc_refs:\n- rc-field-title", "doc_refs:\n- nonexistent-entry\n- rc-field-title")
	}, crit: []string{"docs.ref_unresolved"}},
	{name: "doc_refs names another capability's entry", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, capReserve, "doc_refs:\n- rc-field-title", "doc_refs:\n- cft-risk\n- rc-field-title")
	}, crit: []string{"docs.ref_wrong_capability"}},
	{name: "documentation entry not referenced", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, capReserve, "- rc-risk") }, crit: []string{"docs.entry_not_referenced"}},
	{name: "document missing", edit: func(t *testing.T, p pkgFiles) { p.remove(t, "documentation/reservations/create.md") }, crit: []string{"docs.location_missing"}},
	{name: "entry documents an unknown capability", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, docIndex, "  capability_id: catalog.find_title", "  capability_id: ghost.capability")
	}, crit: []string{"docs.entry_unknown_capability"}, loose: true},
	{name: "entry documents a field that does not exist", edit: func(t *testing.T, p pkgFiles) { p.edit(t, docIndex, "  field: notes", "  field: memo") }, crit: []string{"docs.facet_missing"}, warn: []string{"docs.subject_unknown"}},
	{name: "no documentation index", edit: func(t *testing.T, p pkgFiles) { p.remove(t, docIndex) }, crit: []string{"docs.index_missing", "docs.facet_missing"}, loose: true},
	{name: "duplicate documentation entry id", edit: func(t *testing.T, p pkgFiles) { p.edit(t, docIndex, "- id: rc-risk", "- id: rc-confirmation") }, crit: []string{"unique.duplicate_id"}, loose: true},

	// ---- conversation contracts (14.11, 14.12) ----
	{name: "required field has no prompt", edit: func(t *testing.T, p pkgFiles) { p.dropBlock(t, ctrRsvEN, "- field: pickup_date") }, crit: []string{"contract.field_prompt_missing"}},
	{name: "prompt for an undeclared field", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "- field: notes\n  label", "- field: memo\n  label")
	}, warn: []string{"contract.field_prompt_unknown_field"}},
	{name: "ask_user rule without a clarification", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "clarifications:\n- rule_id: title_multiple\n  prompt: I found more than one match for {input.title}. Which one do you mean?\n", "clarifications: []\n")
	}, crit: []string{"contract.clarification_missing"}},
	{name: "clarification for a rule that does not exist", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "- rule_id: title_multiple", "- rule_id: title_other")
	}, crit: []string{"contract.clarification_missing"}, warn: []string{"contract.clarification_unknown_rule"}},
	{name: "failure state without a response", edit: func(t *testing.T, p pkgFiles) { p.dropBlock(t, ctrRsvEN, "- code: stale") }, crit: []string{"contract.failure_missing"}},
	{name: "response for an undeclared failure", edit: func(t *testing.T, p pkgFiles) { p.edit(t, ctrRsvEN, "- code: stale\n", "- code: unavailable\n") }, crit: []string{"contract.failure_missing"}, warn: []string{"contract.failure_unknown"}},
	{name: "confirmation capability without confirmations", edit: func(t *testing.T, p pkgFiles) { p.dropBlock(t, ctrRsvEN, "confirmations:") }, crit: []string{"contract.confirmations_mismatch"}, loose: true},
	{name: "corrections without the invalidation wording", edit: func(t *testing.T, p pkgFiles) {
		p.dropLine(t, ctrRsvEN, "prepared_action_invalidated: I changed the details, so the earlier summary no longer applies. Here is the new one.")
	}, crit: []string{"contract.correction_wording_missing"}, loose: true},
	{name: "success outcome that is not documented", edit: func(t *testing.T, p pkgFiles) { p.edit(t, ctrRsvEN, "- outcome: reserved", "- outcome: booked") }, crit: []string{"contract.outcome_mismatch", "docs.facet_missing"}},
	{name: "next action without a label", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "next_actions:\n- id: find_another\n  label: Look for another title", "next_actions: []")
	}, crit: []string{"contract.next_action_label_missing"}},
	{name: "correctable field that does not exist", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "  - notes\n  prepared_action", "  - memo\n  prepared_action")
	}, crit: []string{"contract.correctable_field_unknown"}},
	{name: "contract relies on an undefined term", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "- title\n- reservation\nfield_prompts", "- title\n- ghost\nfield_prompts")
	}, crit: []string{"contract.terminology_unknown"}},
	{name: "term without a label in the contract language", edit: func(t *testing.T, p pkgFiles) { p.dropBlock(t, termFile, "it:") }, crit: []string{"contract.terminology_label_missing"}, warn: []string{"terminology.language_missing"}},
	{name: "unmatched brace in a template", edit: func(t *testing.T, p pkgFiles) { p.edit(t, ctrRsvEN, "{input.title}. Which", "{input.title. Which") }, crit: []string{"contract.template_malformed"}},
	{name: "placeholder for a fact that is not declared", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, ctrRsvEN, "{reservation.title} at", "{reservation.colour} at")
	}, crit: []string{"contract.placeholder_unknown"}, loose: true},
	{name: "placeholder for an undeclared input", edit: func(t *testing.T, p pkgFiles) { p.edit(t, ctrRsvEN, "{input.title}. Which", "{input.titel}. Which") }, crit: []string{"contract.placeholder_unknown"}},
	{name: "obligation no response states", edit: func(t *testing.T, p pkgFiles) { p.edit(t, ctrRsvEN, " Your reference is {reservation.reference}.", "") }, crit: []string{"contract.obligation_not_stated"}},
	{name: "contract for an undeclared capability", edit: func(t *testing.T, p pkgFiles) {
		p.set("conversation-contracts/ghost.en.yaml", strings.Replace(string(p[ctrRsvEN]), "capability_id: reservations.create", "capability_id: ghost.capability", 1))
	}, crit: []string{"contract.unknown_capability"}, loose: true},
	{name: "contract in an unsupported language", edit: func(t *testing.T, p pkgFiles) {
		p.set("conversation-contracts/reservations-create.fr.yaml", strings.Replace(string(p[ctrRsvEN]), "language: en", "language: fr", 1))
	}, crit: []string{"contract.language_unsupported"}},
	{name: "two contracts for one capability and language", edit: func(t *testing.T, p pkgFiles) {
		p.set("conversation-contracts/reservations-create.en2.yaml", string(p[ctrRsvEN]))
	}, crit: []string{"unique.duplicate_id"}},

	// ---- policy (14.13) ----
	{name: "policy grants an undeclared role", edit: func(t *testing.T, p pkgFiles) { p.edit(t, policyFile, "- role: visitor", "- role: guest") }, crit: []string{"policy.unknown_reference"}},
	{name: "policy grants an undeclared capability", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, policyFile, "  - reservations.create\n- role: visitor", "  - reservations.destroy\n- role: visitor")
	}, crit: []string{"policy.unknown_reference"}},
	{name: "projection of an undeclared field", edit: func(t *testing.T, p pkgFiles) { p.edit(t, policyFile, "  - pickup_date", "  - pickup_dat") }, crit: []string{"policy.unknown_reference", "policy.required_field_hidden"}},
	{name: "projection hides a required field", edit: func(t *testing.T, p pkgFiles) { p.dropLine(t, policyFile, "- pickup_date") }, crit: []string{"policy.required_field_hidden"}},
	{name: "role granted twice across policies", edit: func(t *testing.T, p pkgFiles) {
		p.set("policies/second.yaml", "policy_id: second\nrole_grants:\n- role: member\n  capabilities:\n  - catalog.find_title\nrisk_overrides: []\nfield_projection: []\n")
	}, crit: []string{"policy.conflict"}},
	{name: "policy id used twice", edit: func(t *testing.T, p pkgFiles) { p.set("policies/copy.yaml", string(p[policyFile])) }, crit: []string{"policy.conflict"}},
	{name: "two risk overrides for one capability", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, policyFile, "risk_overrides: []", "risk_overrides:\n- capability_id: reservations.create\n  minimum_risk_level: 3\n  reason: Raised for a test.")
		p.set("policies/second.yaml", "policy_id: second\nrole_grants:\n- role: visitor\n  capabilities:\n  - catalog.find_title\nrisk_overrides:\n- capability_id: reservations.create\n  minimum_risk_level: 3\n  reason: Raised again.\nfield_projection: []\n")
		p.edit(t, policyFile, "- role: visitor\n  capabilities:\n  - catalog.find_title\n", "")
	}, crit: []string{"policy.conflict"}},
	{name: "risk override that raises nothing", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, policyFile, "risk_overrides: []", "risk_overrides:\n- capability_id: reservations.create\n  minimum_risk_level: 1\n  reason: Not higher than the declared level.")
	}, warn: []string{"policy.override_not_higher"}},
	{name: "two projections for the same roles", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, policyFile, "  - pickup_date\n", "  - pickup_date\n- capability_id: reservations.create\n  roles:\n  - member\n  model_visible_fields:\n  - title\n  - pickup_date\n")
	}, crit: []string{"policy.conflict"}},
	{name: "rule hides a required field", edit: func(t *testing.T, p pkgFiles) {
		p.set(policyFile, string(p[policyFile])+"format: policy/v1\nrules:\n- id: hide-title\n  capabilities:\n  - reservations.create\n  when:\n    roles:\n      any:\n      - visitor\n  then:\n    hide_fields:\n    - title\n  reason: Test rule.\n")
	}, warn: []string{"policy.rule_hides_required_field"}},
	{name: "rule hides a field that does not exist", edit: func(t *testing.T, p pkgFiles) {
		p.set(policyFile, string(p[policyFile])+"format: policy/v1\nrules:\n- id: hide-colour\n  capabilities:\n  - reservations.create\n  when:\n    roles:\n      any:\n      - member\n  then:\n    hide_fields:\n    - colour\n  reason: Test rule.\n")
	}, crit: []string{"policy.unknown_reference"}},
	{name: "rule for an undeclared role", edit: func(t *testing.T, p pkgFiles) {
		p.set(policyFile, string(p[policyFile])+"format: policy/v1\nrules:\n- id: deny-guests\n  capabilities:\n  - reservations.create\n  when:\n    roles:\n      any:\n      - guest\n  then:\n    deny: true\n  reason: Test rule.\n")
	}, crit: []string{"policy.unknown_reference"}},
	{name: "a policy file the runtime parser refuses", edit: func(t *testing.T, p pkgFiles) {
		p.set(policyFile, string(p[policyFile])+"format: policy/v2\n")
	}, crit: []string{"schema.policy"}},

	// ---- terminology (14.14) ----
	{name: "term for an undeclared entity type", edit: func(t *testing.T, p pkgFiles) { p.edit(t, termFile, "  entity_type: title", "  entity_type: book") }, crit: []string{"terminology.entity_type_unknown"}},
	{name: "duplicate term id", edit: func(t *testing.T, p pkgFiles) { p.edit(t, termFile, "- id: reservation", "- id: title") }, crit: []string{"unique.duplicate_id"}, loose: true},

	// ---- RAG sources (14.15) ----
	{name: "source egress entry not in the manifest", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, srcFile, "egress:\n  entries: []", "egress:\n  entries:\n  - ghost")
	}, crit: []string{"rag.egress_unknown"}},
	{name: "package root that holds no files", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, srcFile, "path: documentation/policies", "path: documentation/missing")
	}, crit: []string{"rag.root_invalid"}},
	{name: "sync event on a transport that is not events", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, srcFile, "sync:\n  method: manual", "sync:\n  method: event\n  event:\n    endpoint: library-mcp\n    event_type: catalogue.changed")
	}, crit: []string{"rag.sync_invalid"}},

	// ---- tests (14.16) ----
	{name: "a safe-execution probe kind missing", edit: func(t *testing.T, p pkgFiles) { p.dropBlock(t, testsFile, "- id: reserve-uncertain") }, crit: []string{"tests.safety_probe_missing"}},
	{name: "no scope probe for a capability", edit: func(t *testing.T, p pkgFiles) {
		p.dropBlock(t, testsFile, "- id: visitor-searches-catalogue")
		p.dropBlock(t, testsFile, "- id: search-never-surfaces-other-network-title")
	}, crit: []string{"tests.capability_unprobed"}},
	{name: "no retrieve probe for a source", edit: func(t *testing.T, p pkgFiles) { p.dropBlock(t, testsFile, "- id: member-retrieves-policies") }, crit: []string{"tests.source_unprobed"}},
	{name: "probe by a principal that is not declared", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "  principal: member-a1\n  target_record: title-a1", "  principal: ghost\n  target_record: title-a1")
	}, crit: []string{"tests.ref_unresolved"}},
	{name: "probe of a record that is not declared", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "target_record: title-a1", "target_record: title-zz")
	}, crit: []string{"tests.ref_unresolved"}},
	{name: "principal carries an undeclared role", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "    roles:\n    - visitor", "    roles:\n    - ghost")
	}, crit: []string{"tests.ref_unresolved"}},
	{name: "principal in a workspace of another tenant", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "  - ref: member-a2\n    tenant: network-a", "  - ref: member-a2\n    tenant: network-b")
	}, crit: []string{"tests.fixture_inconsistent"}},
	{name: "seed data file missing", edit: func(t *testing.T, p pkgFiles) { p.remove(t, "tests/seed/library.json") }, crit: []string{"tests.ref_unresolved"}},
	{name: "provisioning call to an undeclared endpoint", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "    create_tenant:\n      endpoint: library-mcp", "    create_tenant:\n      endpoint: ghost")
	}, crit: []string{"endpoint.unknown"}},
	{name: "provisioning call with the wrong key", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "      endpoint: library-mcp\n      mcp_tool: test_seed", "      endpoint: library-mcp\n      native_method: test_seed")
	}, crit: []string{"endpoint.transport_mismatch"}},
	{name: "no test suite", edit: func(t *testing.T, p pkgFiles) { p.remove(t, testsFile, "tests/seed/library.json") }, crit: []string{"tests.none", "tests.capability_unprobed", "tests.source_unprobed", "tests.safety_probe_missing"}},
	{name: "safe-execution probe of a read-only capability", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "conversation_cases:", "- id: find-confirmed\n  capability_id: catalog.find_title\n  principal: member-a1\n  kind: confirmed_commit\n  expect:\n    outcome: committed_once\nconversation_cases:")
	}, warn: []string{"tests.safety_probe_unneeded"}},
	{name: "duplicate suite id", edit: func(t *testing.T, p pkgFiles) { p.set("tests/second.yaml", string(p[testsFile])) }, crit: []string{"unique.duplicate_id"}, loose: true},
	{name: "duplicate probe id", edit: func(t *testing.T, p pkgFiles) { p.edit(t, testsFile, "- id: reserve-stale", "- id: reserve-expired") }, crit: []string{"unique.duplicate_id"}},
	{name: "conversation case in an unsupported language", edit: func(t *testing.T, p pkgFiles) {
		p.edit(t, testsFile, "- id: reserve-missing-date\n  language: en", "- id: reserve-missing-date\n  language: fr")
	}, crit: []string{"tests.ref_unresolved"}},
}

// TestEveryDefectHasItsFinding builds each defect into the valid package and
// checks the findings it raises: severity, check ID, and that the finding is
// complete (message and remediation) and names the capability when there is one.
func TestEveryDefectHasItsFinding(t *testing.T) {
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			files := valid(t)
			m.edit(t, files)
			fsys := files.fsys()
			if !m.unsigned {
				fsys = files.signed(t)
			}
			res := mustLoad(t, fsys, loadOptions())
			crit := ids(res.Findings, contracts.SeverityCritical)
			if m.loose {
				for _, want := range m.crit {
					if !contains(crit, want) {
						t.Fatalf("critical checks = %v, want to include %s\n%s", crit, want, dump(res.Findings))
					}
				}
			} else {
				requireCritical(t, res, m.crit...)
			}
			if len(m.crit) == 0 && (res.Critical() || res.Package == nil) {
				t.Fatalf("a defect with warnings only must still load:\n%s", dump(res.Findings))
			}
			warn := ids(res.Findings, contracts.SeverityWarning)
			for _, want := range m.warn {
				if !contains(warn, want) {
					t.Fatalf("warning checks = %v, want to include %s\n%s", warn, want, dump(res.Findings))
				}
			}
			for _, f := range res.Findings {
				if err := f.Validate(); err != nil {
					t.Errorf("incomplete finding %v: %v", f, err)
				}
			}
		})
	}
}

// TestChecksRegistry: every check has its wording, severity and a clause of
// the specification; IDs are stable dotted lower-case names; the registry is
// what Checks reports.
func TestChecksRegistry(t *testing.T) {
	idRE := regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	all := Checks()
	if len(all) < 80 {
		t.Fatalf("only %d checks registered", len(all))
	}
	seen := map[string]bool{}
	for i, c := range all {
		if !idRE.MatchString(c.ID) {
			t.Errorf("check ID %q is not a dotted lower-case name", c.ID)
		}
		if seen[c.ID] {
			t.Errorf("check ID %q listed twice", c.ID)
		}
		seen[c.ID] = true
		if !c.Severity.Valid() || c.Summary == "" || c.Remediation == "" {
			t.Errorf("check %s is incomplete: %+v", c.ID, c)
		}
		if i > 0 && all[i-1].ID >= c.ID {
			t.Errorf("Checks is not sorted at %s", c.ID)
		}
		if strings.Contains(strings.ToLower(c.Remediation), "llm") {
			t.Errorf("remediation of %s speaks of an LLM: the loader never asks one", c.ID)
		}
	}
}

// TestMutationsCoverTheRegistry keeps the table above honest: apart from the
// checks that need a structure the minimal package does not have, each
// registered check is raised by some mutation or by a dedicated test.
func TestMutationsCoverTheRegistry(t *testing.T) {
	// raised by dedicated tests (archive, signature, install or fuzz) or only
	// reachable in a package the fixture is not
	elsewhere := map[string]bool{
		"package.unsafe_entry": true, "package.limit_exceeded": true, "archive.invalid": true, "archive.compression_ratio": true,
		"load.findings_truncated": true,
		"signature.missing":       true, "signature.invalid": true, "signature.tampered": true, "signature.binding_mismatch": true,
		"signature.malformed": true, "signature.unverifiable": true, "signature.not_checked": true,
		"manifest.reserved_application_id": true, "compat.range_invalid": true, "compat.core_out_of_range": true, "compat.core_version_unknown": true,
		"yaml.invalid":                     true,
		"capability.risk_below_floor":      true, // the capability schema already refuses it; kept as defence in depth
		"contract.step_up_wording_missing": true, // needs a step_up capability
		"contract.for_admin_capability":    true,
		"capability.adapter_mapping":       true, "contract.missing": true, "schema.rag_source": true,
		"rag.scope_application_mismatch": true, // TestRAGSourceMissingScopeIsCritical
		// the runtime parser and the embedded schema accept the same policy
		// documents (TestPolicySchemaAndRuntimeParserAgree), so this is defence in depth
		"policy.invalid": true,
	}
	covered := map[string]bool{}
	for _, m := range mutations {
		for _, c := range append(append([]string(nil), m.crit...), m.warn...) {
			covered[c] = true
		}
	}
	for _, c := range Checks() {
		if !covered[c.ID] && !elsewhere[c.ID] {
			t.Errorf("check %s is raised by no mutation and no dedicated test", c.ID)
		}
	}
}
