package apppkg

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// check is one deterministic check of the loader. Its ID is the stable
// check_id of the Findings it produces; its severity and remediation wording
// are fixed here, so a finding never carries advice composed from package
// content. The same registry feeds Checks (for the audit report and the
// documentation) and the test that every ID has remediation text.
type check struct {
	id          string
	severity    contracts.Severity
	summary     string
	remediation string
	// ref is the clause of docs/integration/application-package.md section 14
	// or of master plan 4.5 that the check implements.
	ref string
}

var registry = map[string]*check{}

func reg(id string, sev contracts.Severity, ref, summary, remediation string) *check {
	if _, dup := registry[id]; dup {
		panic("apppkg: check " + id + " registered twice")
	}
	c := &check{id: id, severity: sev, ref: ref, summary: summary, remediation: remediation}
	registry[id] = c
	return c
}

const (
	crit = contracts.SeverityCritical
	warn = contracts.SeverityWarning
	info = contracts.SeverityInfo
)

// CheckInfo describes one registered check.
type CheckInfo struct {
	ID          string
	Severity    contracts.Severity
	Reference   string
	Summary     string
	Remediation string
}

// Checks returns every check the loader can report, sorted by ID. The
// integration audit (M1-17) lists the static checks it inherits from here.
func Checks() []CheckInfo {
	out := make([]CheckInfo, 0, len(registry))
	for _, c := range registry {
		out = append(out, CheckInfo{ID: c.id, Severity: c.severity, Reference: c.ref, Summary: c.summary, Remediation: c.remediation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Package structure and archives (section 14 item 1).
var (
	chkUnsafeEntry = reg("package.unsafe_entry", crit, "14.1, SEC-059",
		"an entry of the package or archive is a link, a special file, a hidden or unsafely named path, a duplicate or a case collision",
		"Remove the entry and rebuild the package: only regular files and directories whose names use letters, digits, '.', '_' and '-' and do not start with a dot are accepted, and no two names may differ only by case.")
	chkLimit = reg("package.limit_exceeded", crit, "14.1, SEC-059",
		"the package exceeds a size, file-count or depth limit of the loader",
		"Reduce the package: split large documents out or remove unused files.")
	chkArchiveInvalid = reg("archive.invalid", crit, "14.1",
		"the file is not a well-formed single gzip-compressed tar stream",
		"Rebuild the archive with 'tar czf' from the package directory, as one gzip member with no trailing data.")
	chkArchiveRatio = reg("archive.compression_ratio", crit, "14.1, SEC-059",
		"the archive expands far beyond its compressed size",
		"Rebuild the archive without the highly compressible padding; legitimate packages compress at most about 100 to 1.")
	chkUnexpectedFile = reg("package.unexpected_file", warn, "2",
		"a file lies outside the package layout and is not used by anything",
		"Move the file into the directory that owns it or remove it. It is signed with the package but never read.")
	chkTruncated = reg("load.findings_truncated", warn, "",
		"the report was cut at the findings limit",
		"Fix the findings listed and validate again; more were found than are shown.")
)

// YAML and schemas (section 14 item 1).
var (
	chkManifestMissing = reg("manifest.missing", crit, "2, 14.1",
		"the package has no alixai.yaml at its root",
		"Add alixai.yaml at the package root.")
	chkYAML = reg("yaml.invalid", crit, "2, 14.1",
		"a file breaks the plain-data authoring rules of section 2 or is not parseable YAML",
		"Rewrite the file as plain YAML: UTF-8, LF line endings, one document, unique string keys, no anchors, aliases, merge keys or tags, and quote any text that looks like a number, date or boolean.")
	chkSchemaManifest = reg("schema.manifest", crit, "14.1",
		"alixai.yaml does not match the manifest schema",
		"Correct alixai.yaml so that it validates against schemas/application-package/manifest.schema.json.")
	chkSchemaCapability = reg("schema.capability", crit, "14.1",
		"a capability file does not match the capability schema",
		"Correct the capability file so that it validates against schemas/application-package/capability.schema.json.")
	chkSchemaContract = reg("schema.conversation_contract", crit, "14.1",
		"a conversation contract does not match its schema",
		"Correct the conversation contract so that it validates against schemas/application-package/conversation-contract.schema.json.")
	chkSchemaDocIndex = reg("schema.documentation_index", crit, "14.1",
		"documentation/index.yaml does not match the documentation-index schema",
		"Correct the documentation index so that it validates against schemas/application-package/documentation-index.schema.json.")
	chkSchemaPolicy = reg("schema.policy", crit, "14.1",
		"a policy file does not match the policy schema",
		"Correct the policy file so that it validates against schemas/application-package/policy.schema.json.")
	chkSchemaTerminology = reg("schema.terminology", crit, "14.1",
		"a terminology file does not match the terminology schema",
		"Correct the terminology file so that it validates against schemas/application-package/terminology.schema.json.")
	chkSchemaRAG = reg("schema.rag_source", crit, "14.1, 14.15, master plan 12.2",
		"a RAG source file does not match the rag-source schema, so a master plan 12.2 field such as the scope is missing or malformed",
		"Declare every field of master plan 12.2 in the source file: owner, scope, root, include and exclude rules, credential reference, formats, limits, sync, deletion, parser, citation, egress and retention.")
	chkSchemaTests = reg("schema.package_tests", crit, "14.1",
		"a test suite file does not match the package-tests schema",
		"Correct the test suite so that it validates against schemas/application-package/package-tests.schema.json.")
	chkReservedApp = reg("manifest.reserved_application_id", crit, "14.2, ADR-0013",
		"the manifest declares the application ID reserved for AlixAI's own signing keys",
		"Choose another application.id: alixai.platform belongs to the platform and is never an application.")
)

// Authenticity (section 14 item 2, master plan 4.5 check 1).
var (
	chkSigMissing = reg("signature.missing", crit, "14.2, SEC-057",
		"the package carries no signature manifest, so it is unsigned",
		"Sign the package with 'alixai package sign' using a publisher key registered for the application.")
	chkSigInvalid = reg("signature.invalid", crit, "14.2, SEC-057",
		"no signature of the package is acceptable: unknown, unlisted, revoked, retired, expired, future-dated or not verifying",
		"Sign the package again with a publisher key that is registered for this application, listed in integrity.publisher_key_ids, not revoked and not expired.")
	chkSigTampered = reg("signature.tampered", crit, "14.2, SEC-057",
		"the package content differs from what was signed: a file was changed, added or removed",
		"Do not edit a signed package. Sign it again after every change, or reinstall the original.")
	chkSigBinding = reg("signature.binding_mismatch", crit, "14.2",
		"the signature is bound to another application or package version than the manifest declares",
		"Sign the package for the application.id and package.version that alixai.yaml declares.")
	chkSigMalformed = reg("signature.malformed", crit, "14.2",
		"the signature manifest does not match its schema",
		"Regenerate signatures/signature-manifest.json with 'alixai package sign'.")
	chkSigUnverifiable = reg("signature.unverifiable", crit, "14.2",
		"the trust store could not be consulted, so the signature cannot be established",
		"Retry when the trust store is available; nothing is trusted meanwhile.")
	chkSigNotChecked = reg("signature.not_checked", warn, "14.2",
		"no trust anchor was supplied, so the content digests were checked but the signer was not established",
		"Validate with a trust store or with -trust-key KEYID=PUBLICKEY to establish who signed the package. An install always requires it.")
)

// Compatibility, consistency and uniqueness (items 3 to 6).
var (
	chkCompatRange = reg("compat.range_invalid", crit, "14.3",
		"compatibility.alixai_core.min is not lower than max_exclusive",
		"Make min lower than max_exclusive.")
	chkCompatCore = reg("compat.core_out_of_range", crit, "14.3",
		"the installed AlixAI core version is outside the package's compatibility range",
		"Install a package built for this core version, or upgrade or downgrade the core to one inside [min, max_exclusive).")
	chkCoreVersion = reg("compat.core_version_unknown", crit, "14.3",
		"the installed core version is not a semantic version, so compatibility cannot be decided",
		"Run the loader with a core version in MAJOR.MINOR.PATCH form.")
	chkDuplicate = reg("unique.duplicate_id", crit, "14.4",
		"an ID that must be unique is used twice (term, documentation entry, policy, suite, field, ambiguity rule, validation rule, next action, example or conversation contract)",
		"Give each object its own ID, and keep at most one conversation contract per capability and language.")
	chkLangDefault = reg("manifest.language_default_unsupported", crit, "14.5",
		"languages.default is not one of languages.supported",
		"Add the default language to languages.supported, or change the default.")
	chkCapFileMissing = reg("manifest.capability_file_missing", crit, "14.5",
		"a capability the manifest lists has no file at the listed path",
		"Add the capability file or remove the capability from the manifest.")
	chkCapIDMismatch = reg("manifest.capability_id_mismatch", crit, "14.5",
		"a capability file declares another id than the key the manifest lists it under",
		"Make the capability's id equal to the key in the manifest's capabilities map.")
	chkCapUnlisted = reg("manifest.capability_file_unlisted", crit, "14.5",
		"a file under capabilities/ is not listed in the manifest, which is the deny-by-default registry",
		"List the capability in the manifest or remove the file.")
	chkRAGFileMissing = reg("manifest.rag_source_file_missing", crit, "14.5",
		"a RAG source the manifest lists has no file at the listed path",
		"Add the source file or remove the source from the manifest.")
	chkRAGIDMismatch = reg("manifest.rag_source_id_mismatch", crit, "14.5",
		"a RAG source file declares another id than the key the manifest lists it under",
		"Make the source's id equal to the key in the manifest's rag_sources map.")
	chkRAGUnlisted = reg("manifest.rag_source_file_unlisted", crit, "14.5",
		"a file under rag-sources/ is not listed in the manifest, so the source is undeclared",
		"List the source in the manifest or remove the file.")
	chkEndpointUnknown = reg("endpoint.unknown", crit, "14.6",
		"a source, citation, resolver, test or event reference names an endpoint the manifest does not declare",
		"Declare the endpoint in the manifest or correct the reference.")
	chkEndpointTransport = reg("endpoint.transport_mismatch", crit, "14.6",
		"an adapter call or reference does not fit the transport of the endpoint it names (an events endpoint cannot own a call, and the call key must match mcp, openapi or native)",
		"Use mcp_tool for an mcp endpoint, openapi_operation_id for an openapi endpoint and native_method for a native endpoint; events endpoints appear only as event sources.")
	chkEndpointFile = reg("endpoint.file_missing", crit, "14.6",
		"an adapter_config, OpenAPI spec or other file an endpoint references does not exist in the package",
		"Add the file under schemas/ or correct the path.")
	chkEgress = reg("endpoint.egress_missing", crit, "14.6",
		"the host and port of an endpoint, loopback included, have no matching network.egress entry",
		"Add a network.egress entry with the exact host, port and scheme. A loopback endpoint needs one too, so the operator sees every loopback port the package asks for; the platform's own ports are refused at the dialer even when listed.")
	chkLookalike = reg("adapter.lookalike_operation", crit, "SEC-052",
		"two different operation names on one endpoint differ only by case or by '_', '-' and '.', so one could be mistaken for the other",
		"Map capabilities to exact, clearly distinct operation names; rename the operation on the application side if it collides.")
	chkSharedOperation = reg("adapter.operation_shared", warn, "SEC-052",
		"one adapter operation is mapped by more than one capability",
		"Confirm that the capabilities really share the operation; otherwise map each capability to its own.")
)

// Capabilities (items 7 to 9).
var (
	chkAdapterMapping = reg("capability.adapter_mapping", crit, "9.2, 14.6, SEC-052, master plan 4.5 check 7",
		"a capability operation has no exact adapter mapping: its endpoint is not declared or the call does not match the endpoint's transport",
		"Map every operation of the capability to a declared mcp, openapi or native endpoint using the call key of that transport.")
	chkResultSchema = reg("capability.result_schema_missing", crit, "14.6, master plan 4.5 check 2",
		"the result_schema file of an operation does not exist, so the deterministic result cannot be validated",
		"Add the result schema below schemas/ or correct the path.")
	chkEntityType = reg("capability.entity_type_unknown", crit, "14.7",
		"an entity_ref field names an entity type the capability does not declare",
		"Declare the entity type in the capability's entity_types or correct the field.")
	chkDefaultType = reg("capability.default_type_mismatch", crit, "14.7",
		"a field default does not fit the type of the field",
		"Give the default a value of the field's type, an allowed enum value, or remove it.")
	chkConstraint = reg("capability.constraint_misapplied", crit, "14.7",
		"a constraint (min_length, minimum, max_items, values ...) is used on a field type it does not apply to, or contradicts itself",
		"Use each constraint only on the field types it is defined for, with consistent bounds.")
	chkAmbiguity = reg("capability.ambiguity_invalid", crit, "14.7",
		"an ambiguity rule names an unknown field, or uses use_default on a field with no default",
		"Point the rule at a declared field and give the field a deterministic default if the rule uses it.")
	chkExample = reg("capability.example_invalid", crit, "14.7",
		"an example names a field the capability does not declare",
		"Use only declared field names in expected_fields.")
	chkExampleLang = reg("capability.example_language_unsupported", warn, "14.7",
		"an example is written in a language the manifest does not support",
		"Use a supported language or add it to the manifest.")
	chkNextAction = reg("capability.next_action_invalid", crit, "14.7",
		"a next action points at a capability that is not declared or is not an audience user capability",
		"Point next actions only at declared user capabilities.")
	chkValidation = reg("capability.validation_invalid", crit, "14.8",
		"a validation rule names an unknown field, or an error code that is not one of the capability's failure_states",
		"Name declared fields and list the rule's error_code in failure_states.")
	chkIdempotency = reg("capability.idempotency_window", crit, "14.9",
		"idempotency.window_seconds is shorter than prepared_action.ttl_seconds",
		"Make the idempotency window at least as long as the prepared action's time to live.")
	chkRiskFloor = reg("capability.risk_below_floor", crit, "9.2, master plan 11.4",
		"the declared risk level is lower than the platform floor of the capability's effect class",
		"Raise risk_level to at least the floor of effect_class (none 0, reversible 1, external 2, privileged 3).")
)

// Documentation (item 10, master plan 4.5 check 4).
var (
	chkDocFacet = reg("docs.facet_missing", crit, "14.10, master plan 4.5 check 4",
		"the documentation index has no entry, in the default language, for a field, validation rule, permission, failure state, the risk level, the confirmation rule or a user-visible outcome of the capability",
		"Add the missing documentation entry to documentation/index.yaml and list its id in the capability's doc_refs. Missing documentation fails the capability; it is never guessed.")
	chkDocRef = reg("docs.ref_unresolved", crit, "14.10",
		"a doc_refs entry of a capability does not exist in the documentation index",
		"Add the entry to the index or remove the reference.")
	chkDocRefCap = reg("docs.ref_wrong_capability", crit, "14.10",
		"a doc_refs entry names a documentation entry that belongs to another capability",
		"Reference only entries whose capability_id is this capability.")
	chkDocUnref = reg("docs.entry_not_referenced", crit, "14.10",
		"a documentation entry of the capability is missing from the capability's doc_refs",
		"Add the entry's id to doc_refs or remove the entry.")
	chkDocLocation = reg("docs.location_missing", crit, "14.10",
		"the document a documentation entry points at does not exist in the package",
		"Add the document below documentation/ or correct location.path.")
	chkDocIndexMissing = reg("docs.index_missing", crit, "14.10, master plan 4.5 check 4",
		"the package has no documentation/index.yaml, so no capability is documented",
		"Add documentation/index.yaml with an entry for every facet of every capability.")
	chkDocCapability = reg("docs.entry_unknown_capability", crit, "14.10",
		"a documentation entry belongs to a capability the manifest does not declare",
		"Correct the entry's capability_id or remove the entry.")
	chkDocSubject = reg("docs.subject_unknown", warn, "14.10",
		"a documentation entry documents a field, rule, permission, failure state or outcome the capability does not have",
		"Correct the entry's subject or remove it.")
)

// Conversation contracts (items 11 and 12, master plan 4.5 check 5).
var (
	chkContractMissing = reg("contract.missing", crit, "14.11, master plan 4.5 check 5",
		"an audience user capability has no conversation contract in a supported language",
		"Add conversation-contracts/<capability>.<language>.yaml for every supported language.")
	chkContractCapability = reg("contract.unknown_capability", crit, "14.11",
		"a conversation contract belongs to a capability the manifest does not declare",
		"Correct the contract's capability_id or remove the file.")
	chkContractLang = reg("contract.language_unsupported", crit, "14.11",
		"a conversation contract is written in a language the manifest does not support",
		"Add the language to languages.supported or remove the contract.")
	chkContractAdmin = reg("contract.for_admin_capability", warn, "14.11",
		"an audience admin capability has a conversation contract, but admin capabilities are never offered in conversation",
		"Remove the contract or change the capability's audience to user.")
	chkFieldPrompt = reg("contract.field_prompt_missing", crit, "14.11",
		"a required field of the capability has no prompt in a conversation contract",
		"Add a field_prompts entry for every required field.")
	chkFieldPromptExtra = reg("contract.field_prompt_unknown_field", warn, "14.11",
		"a field prompt names a field the capability does not declare",
		"Remove the prompt or declare the field.")
	chkClarification = reg("contract.clarification_missing", crit, "14.11",
		"an ambiguity rule with action ask_user has no clarification in a conversation contract",
		"Add a clarifications entry with the rule's id.")
	chkClarificationExtra = reg("contract.clarification_unknown_rule", warn, "14.11",
		"a clarification names an ambiguity rule the capability does not have or that does not ask the user",
		"Remove the clarification or add the rule.")
	chkFailureResponse = reg("contract.failure_missing", crit, "14.11",
		"a failure state of the capability has no failure response in a conversation contract",
		"Add a failures entry with the code (and application_code) of the failure state. AlixAI never falls back to a generic message.")
	chkFailureExtra = reg("contract.failure_unknown", warn, "14.11",
		"a failure response names a failure state the capability does not declare",
		"Remove the response or declare the failure state.")
	chkConfirmations = reg("contract.confirmations_mismatch", crit, "14.11",
		"confirmations is present when the capability's confirmation mode is none, or absent when it is not",
		"Provide confirmations exactly when confirmation.mode is explicit or step_up.")
	chkStepUpWording = reg("contract.step_up_wording_missing", crit, "14.11",
		"a step_up capability has no confirmations.step_up_required wording",
		"Add confirmations.step_up_required.")
	chkCorrectionWording = reg("contract.correction_wording_missing", crit, "14.11",
		"a capability with a prepare operation has no corrections.prepared_action_invalidated wording",
		"Add corrections.prepared_action_invalidated.")
	chkOutcomes = reg("contract.outcome_mismatch", crit, "14.11",
		"the success outcomes of a conversation contract differ from the user_visible_outcome entries documented for the capability",
		"Make the contract's success outcomes and the documented outcomes the same set.")
	chkNextLabel = reg("contract.next_action_label_missing", crit, "14.11",
		"a next action of the capability has no label in a conversation contract",
		"Add a next_actions entry with the action's id.")
	chkCorrectable = reg("contract.correctable_field_unknown", crit, "14.11",
		"corrections.correctable_fields names a field the capability does not declare",
		"Name only declared fields.")
	chkTermUnknown = reg("contract.terminology_unknown", crit, "14.11",
		"a conversation contract relies on a term that is not defined in terminology/",
		"Define the term or remove it from the contract.")
	chkTermLabel = reg("contract.terminology_label_missing", crit, "14.11",
		"a term a contract relies on has no label in the contract's language",
		"Add a label in that language to the term.")
	chkTemplate = reg("contract.template_malformed", crit, "14.12",
		"a response template has an unmatched brace or an empty or malformed placeholder",
		"Write placeholders as {fact.path} or {input.field} and literal braces as {{ and }}.")
	chkPlaceholder = reg("contract.placeholder_unknown", crit, "14.12",
		"a placeholder names an input field the capability does not declare, or a fact not declared in response_obligations",
		"Use {input.<field>} for declared fields and declare every other fact in response_obligations.")
	chkObligation = reg("contract.obligation_not_stated", crit, "14.12, master plan 9.2",
		"a response obligation is not stated by any response that must state it",
		"Use the fact as a placeholder in a success response (obligations that apply on success or always) and in the confirmation request (prepared or always).")
)

// Policy, terminology, sources and tests (items 13 to 16).
var (
	chkPolicyInvalid = reg("policy.invalid", crit, "14.13, ADR-0017",
		"a policy file is not a valid policy/v1 document, or the policies of the package cannot be compiled into the form the runtime enforces",
		"Correct the policy: it is read by the same parser the runtime uses.")
	chkPolicyRef = reg("policy.unknown_reference", crit, "14.13",
		"a policy names a role that is not in tenancy.roles, a capability that is not declared, or a field the capability does not have",
		"Refer only to roles, capabilities and fields the package declares.")
	chkPolicyConflict = reg("policy.conflict", crit, "14.13",
		"two policy statements overlap: a role granted twice, two risk overrides or two overlapping projections for one capability, a repeated policy or rule id",
		"Keep one statement per role, per capability override and per projection scope.")
	chkPolicyHidden = reg("policy.required_field_hidden", crit, "14.13",
		"a field projection hides a required field from the model, which makes the capability unusable",
		"Include every required field in model_visible_fields.")
	chkPolicyRuleHides = reg("policy.rule_hides_required_field", warn, "14.13",
		"a conditional rule can hide a required field, which makes the capability unusable while the rule applies",
		"Deny the capability explicitly instead of hiding a required field, or confirm the effect is intended.")
	chkPolicyOverride = reg("policy.override_not_higher", warn, "14.13",
		"a risk override is not higher than the capability's declared level and changes nothing",
		"Remove the override or raise it above the declared level.")
	chkTermEntity = reg("terminology.entity_type_unknown", crit, "14.14",
		"a term names an entity type that no capability declares",
		"Use an entity type of some capability or remove entity_type.")
	chkTermLanguage = reg("terminology.language_missing", warn, "14.14",
		"a term has no label in a language the manifest supports",
		"Add a label in the missing language.")
	chkRAGScope = reg("rag.scope_application_mismatch", crit, "14.15",
		"a source's scope.application_id is not the manifest's application.id",
		"Set scope.application_id to the application's id.")
	chkRAGEgress = reg("rag.egress_unknown", crit, "14.15",
		"a source lists an egress entry that network.egress does not declare",
		"Declare the entry in the manifest or remove it from the source.")
	chkRAGRoot = reg("rag.root_invalid", crit, "14.15",
		"a source's root names an endpoint that is not an mcp or openapi endpoint, a URL whose host has no egress entry, or a package folder that holds no files",
		"Point the root at a declared mcp or openapi endpoint, an allowed host, or an existing folder below documentation/.")
	chkRAGSync = reg("rag.sync_invalid", crit, "14.15",
		"a source's sync.event names an endpoint that is not an events endpoint or an event type it does not declare",
		"Name an events endpoint of the manifest and one of its event_types.")
	chkTestRef = reg("tests.ref_unresolved", crit, "14.16",
		"a test suite refers to a tenant, workspace, principal, record, capability, source, role or file that does not exist",
		"Declare the referenced fixture or correct the reference.")
	chkSafetyProbes = reg("tests.safety_probe_missing", crit, "14.16, master plan 4.5 check 8",
		"a consequential capability (risk level 1 or higher) lacks one of the seven safe-execution probe kinds",
		"Add probes of every kind: confirmed_commit, unconfirmed_commit, replayed_commit, expired_preparation, stale_record, cancelled_preparation and uncertain_commit_recovery.")
	chkProbeCapability = reg("tests.capability_unprobed", crit, "14.16, master plan 4.5 check 6",
		"a capability has no scope probe in any test suite",
		"Add positive and negative scope probes for the capability.")
	chkProbeSource = reg("tests.source_unprobed", crit, "14.16, master plan 4.5 check 6",
		"a RAG source has no retrieve scope probe in any test suite",
		"Add a retrieve probe that targets the source.")
	chkProbeExtra = reg("tests.safety_probe_unneeded", warn, "14.16",
		"a safe-execution probe targets a read-only capability",
		"Remove the probe or declare the capability's effect.")
	chkNoTests = reg("tests.none", crit, "14.16, master plan 4.5 checks 6, 8, 10",
		"the package has no test suite, so isolation, safe execution and the smoke test cannot be run",
		"Add at least one tests/*.yaml suite.")
	chkTestsFixtures = reg("tests.fixture_inconsistent", crit, "14.16",
		"a fixture principal or record names a workspace that does not belong to its tenant, or two fixtures share a reference",
		"Make every principal and record refer to a workspace of its own tenant.")
)

// finding collector -------------------------------------------------------

type collector struct {
	max       int
	list      []contracts.Finding
	truncated bool
}

func newCollector(max int) *collector { return &collector{max: max} }

// add records a finding of check ch. The message is formatted from fixed text
// and quoted identifiers; callers pass untrusted strings through q.
func (c *collector) add(ch *check, capability, path, format string, args ...any) {
	if len(c.list) >= c.max {
		c.truncated = true
		return
	}
	c.list = append(c.list, contracts.Finding{
		CheckID:     ch.id,
		Severity:    ch.severity,
		Capability:  capability,
		Path:        clipPath(path),
		Message:     fmt.Sprintf(format, args...),
		Remediation: ch.remediation,
	})
}

// finalize sorts the findings and appends the truncation notice.
func (c *collector) finalize() []contracts.Finding {
	if c.truncated {
		c.list = append(c.list, contracts.Finding{
			CheckID:     chkTruncated.id,
			Severity:    chkTruncated.severity,
			Message:     fmt.Sprintf("more than %d findings were produced; the rest are not shown", c.max),
			Remediation: chkTruncated.remediation,
		})
	}
	contracts.SortFindings(c.list)
	if c.list == nil {
		c.list = []contracts.Finding{}
	}
	return c.list
}

func (c *collector) critical() bool { return contracts.HasCritical(c.list) }

// q quotes an untrusted string for a message: Go-quoted, control characters
// escaped, and clipped to a bounded length so a hostile value cannot flood or
// forge a finding.
func q(s string) string {
	const max = 80
	if len(s) > max {
		cut := max
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return strconv.Quote(s)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// qs quotes and joins a list of untrusted strings.
func qs(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = q(x)
	}
	return strings.Join(parts, ", ")
}
