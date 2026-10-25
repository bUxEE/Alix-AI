package settings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// policyDoc is the shape the tenant and the deployment documents share: defaults
// and policy for everyone below them. The two generated types are different Go
// types of one shape; the rules are written once over this.
type policyDoc struct {
	DefaultLanguage     *gen.LanguageTag
	DefaultModelProfile *gen.ProfileID
	Voice               *gen.PolicyVoice
	History             *gen.PolicyHistory
	Privacy             *gen.PolicyPrivacy
	Attachments         *gen.PolicyAttachments
	Notifications       *gen.PolicyNotifications
	Capabilities        *gen.CapabilitySwitches
	Confirmation        *gen.ConfirmationRaise
}

func policyOfTenant(t gen.TenantSettings) policyDoc {
	return policyDoc{t.DefaultLanguage, t.DefaultModelProfile, t.Voice, t.History, t.Privacy, t.Attachments, t.Notifications, t.Capabilities, t.Confirmation}
}

func policyOfDeployment(d gen.DeploymentSettings) policyDoc {
	return policyDoc{d.DefaultLanguage, d.DefaultModelProfile, d.Voice, d.History, d.Privacy, d.Attachments, d.Notifications, d.Capabilities, d.Confirmation}
}

// document is one decoded document of any level; exactly one of the pointers is set.
type document struct {
	level Level
	user  *gen.UserSettings
	// policy is the tenant or deployment document.
	policy *policyDoc
}

// decodeDocument decodes the JSON of a stored or submitted document into its typed
// form. It does not validate against the schema (a submitted document has been; a
// stored one was before it was stored). An undecodable document is a refusal when
// submitted and an internal fault when stored; the caller decides which.
func decodeDocument(level Level, raw []byte) (document, error) {
	switch level {
	case LevelUser:
		var u gen.UserSettings
		if err := json.Unmarshal(raw, &u); err != nil {
			return document{}, err
		}
		return document{level: level, user: &u}, nil
	case LevelTenant:
		var t gen.TenantSettings
		if err := json.Unmarshal(raw, &t); err != nil {
			return document{}, err
		}
		p := policyOfTenant(t)
		return document{level: level, policy: &p}, nil
	case LevelDeployment:
		var d gen.DeploymentSettings
		if err := json.Unmarshal(raw, &d); err != nil {
			return document{}, err
		}
		p := policyOfDeployment(d)
		return document{level: level, policy: &p}, nil
	}
	return document{}, contracts.New(contracts.ErrInternal, "settings: unknown level")
}

// canonical returns the canonical JSON of the typed document: members in schema
// order, lists sorted, nothing the schema does not name. The same document always
// yields the same bytes, which is what makes "unchanged" and the digest in the
// audit event mean something.
func (d document) canonical() ([]byte, error) {
	switch d.level {
	case LevelUser:
		u := *d.user
		if u.Capabilities != nil {
			c := *u.Capabilities
			c.Disabled = sortedIDs(c.Disabled)
			u.Capabilities = &c
		}
		if u.Notifications != nil && u.Notifications.Events != nil {
			n := *u.Notifications
			n.Events = slices.Clone(n.Events)
			sort.Slice(n.Events, func(i, j int) bool { return n.Events[i] < n.Events[j] })
			u.Notifications = &n
		}
		return json.Marshal(u)
	case LevelTenant:
		return json.Marshal(tenantOfPolicy(d.sortedPolicy()))
	case LevelDeployment:
		return json.Marshal(deploymentOfPolicy(d.sortedPolicy()))
	}
	return nil, contracts.New(contracts.ErrInternal, "settings: unknown level")
}

func (d document) sortedPolicy() policyDoc {
	p := *d.policy
	if p.Capabilities != nil {
		c := *p.Capabilities
		c.Disabled = sortedIDs(c.Disabled)
		p.Capabilities = &c
	}
	return p
}

func tenantOfPolicy(p policyDoc) gen.TenantSettings {
	return gen.TenantSettings{DefaultLanguage: p.DefaultLanguage, DefaultModelProfile: p.DefaultModelProfile, Voice: p.Voice, History: p.History,
		Privacy: p.Privacy, Attachments: p.Attachments, Notifications: p.Notifications, Capabilities: p.Capabilities, Confirmation: p.Confirmation}
}

func deploymentOfPolicy(p policyDoc) gen.DeploymentSettings {
	return gen.DeploymentSettings{DefaultLanguage: p.DefaultLanguage, DefaultModelProfile: p.DefaultModelProfile, Voice: p.Voice, History: p.History,
		Privacy: p.Privacy, Attachments: p.Attachments, Notifications: p.Notifications, Capabilities: p.Capabilities, Confirmation: p.Confirmation}
}

func sortedIDs(in []common.CapabilityID) []common.CapabilityID {
	out := slices.Clone(in)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// capabilityIDs lists the capabilities a document switches off.
func (d document) capabilityIDs() []string {
	var sw *gen.CapabilitySwitches
	if d.user != nil {
		sw = d.user.Capabilities
	} else if d.policy != nil {
		sw = d.policy.Capabilities
	}
	if sw == nil {
		return nil
	}
	out := make([]string, len(sw.Disabled))
	for i, id := range sw.Disabled {
		out[i] = string(id)
	}
	return out
}

// semantic holds what a document is checked against beyond its schema: the
// operator's limits, the languages of the active package and the policy gateway's
// answer for a capability.
type semantic struct {
	limits    Limits
	languages []string
	// mayUse reports whether the caller is itself allowed to use the capability
	// (the policy gateway, expose phase): declared, user-audience, in the token's
	// scopes and granted by the host policy.
	mayUse func(id string) bool
}

// check enforces the rules the schema can not state. Every string a document can
// hold is therefore an element of a closed set the operator or the package
// defined (a profile, a voice, a language, a capability) or an enum; nothing
// arbitrary is ever stored, which is why no secret can be.
func (c semantic) check(d document) error {
	var lang *gen.LanguageTag
	var profile *gen.ProfileID
	var retention *gen.RetentionDays
	var fileMB *gen.FileMegabytes
	switch d.level {
	case LevelUser:
		u := d.user
		lang, profile = u.Language, u.ModelProfile
		if u.Voice != nil && u.Voice.VoiceID != nil && !slices.Contains(c.limits.ApprovedVoices, string(*u.Voice.VoiceID)) {
			return refuse("the voice is not one the operator approved").WithDetails("reason", "voice_not_approved").WithDetails("member", "voice.voice_id")
		}
		if u.History != nil {
			retention = u.History.RetentionDays
		}
		if u.Attachments != nil {
			fileMB = u.Attachments.MaxFileMb
		}
	default:
		p := d.policy
		lang, profile = p.DefaultLanguage, p.DefaultModelProfile
		if p.History != nil {
			retention = p.History.RetentionDays
		}
		if p.Attachments != nil {
			fileMB = p.Attachments.MaxFileMb
		}
	}
	if lang != nil && len(c.languages) > 0 && !slices.Contains(c.languages, string(*lang)) {
		return refuse("the language is not one the application supports").WithDetails("reason", "language_not_supported")
	}
	if profile != nil && !slices.Contains(c.limits.ApprovedProfiles, string(*profile)) {
		return refuse("the model profile is not one the operator approved").WithDetails("reason", "profile_not_approved")
	}
	if retention != nil && int64(*retention) > int64(c.limits.MaxHistoryRetentionDays) {
		return refuse("the history retention is above the operator's ceiling of %d days", c.limits.MaxHistoryRetentionDays).WithDetails("reason", "above_ceiling")
	}
	if fileMB != nil && int64(*fileMB) > int64(c.limits.MaxAttachmentMB) {
		return refuse("the attachment size is above the operator's ceiling of %d MB", c.limits.MaxAttachmentMB).WithDetails("reason", "above_ceiling")
	}
	for i, id := range d.capabilityIDs() {
		if c.mayUse == nil || !c.mayUse(id) {
			// One answer for undeclared, admin-only and out-of-scope alike, so the
			// list of capabilities can not be probed through settings. The id is
			// not echoed; its position is enough for the caller.
			return contracts.New(contracts.ErrUnknownCapability,
				"settings: a capability in capabilities.disabled is not declared by the application or not available to the caller").
				WithDetails("index", i)
		}
	}
	return nil
}

// digest is the lower-case hex SHA-256 of canonical JSON.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// changedSections lists the top-level members whose value differs between two
// canonical documents, sorted. Names only: the audit event never carries values.
func changedSections(oldDoc, newDoc []byte) []string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(oldDoc, &a)
	_ = json.Unmarshal(newDoc, &b)
	seen := map[string]bool{}
	var out []string
	for k, v := range a {
		seen[k] = true
		if nv, ok := b[k]; !ok || !bytes.Equal(v, nv) {
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
