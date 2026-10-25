package rag

import "github.com/buxee/alix-ai/pkg/contracts/gen/conversation"

// This file is hand-written; only files whose first line says "Code generated
// by tools/schemagen" are owned by the generator. It holds the two rules of
// the external-provider seam that a generated type cannot carry, so every
// consumer applies them the same way instead of re-deriving them:
//
//   - a source-metadata value that is missing (or not a listed value) is read
//     as the most restrictive one, never as the registration default;
//   - the client-facing Citation is the internal EvidenceCitation without the
//     two internal members.

// EffectiveSourceEgress is the data-egress mode to enforce for a value that
// may be missing. A missing or unknown value is local_only: content never
// leaves the machine. It is not deny, because deny removes the source from the
// capability entirely and a missing value must not change Local mode; and it is
// never one of the permissive modes.
func EffectiveSourceEgress(v *SourceEgress) SourceEgress {
	if v == nil || !v.Valid() {
		return SourceEgressLocalOnly
	}
	return *v
}

// EffectiveSourceClassification is the classification to enforce for a value
// that may be missing. A missing or unknown value is restricted, the most
// restrictive class. The registration default for a source that states none is
// internal; that default is applied when the source is registered, not here.
func EffectiveSourceClassification(v *SourceClassification) SourceClassification {
	if v == nil || !v.Valid() {
		return SourceClassificationRestricted
	}
	return *v
}

// EffectiveEgress is the egress mode to enforce for this item.
func (v EvidenceItem) EffectiveEgress() SourceEgress { return EffectiveSourceEgress(v.SourceEgress) }

// EffectiveClassification is the classification to enforce for this item.
func (v EvidenceItem) EffectiveClassification() SourceClassification {
	return EffectiveSourceClassification(v.SourceClassification)
}

// EffectiveEgress is the egress mode to enforce for this citation's source.
func (v EvidenceCitation) EffectiveEgress() SourceEgress {
	return EffectiveSourceEgress(v.SourceEgress)
}

// EffectiveClassification is the classification to enforce for this
// citation's source.
func (v EvidenceCitation) EffectiveClassification() SourceClassification {
	return EffectiveSourceClassification(v.SourceClassification)
}

// ClientCitation is the citation a client may receive: every member of the
// internal record except source_egress and source_classification. It is the only
// supported way from an EvidenceCitation to the wire; the client Citation type has
// no member for either value.
func (v EvidenceCitation) ClientCitation() conversation.Citation {
	return conversation.Citation{
		SourceID:     v.SourceID,
		DisplayName:  v.DisplayName,
		Version:      v.Version,
		Locator:      v.Locator,
		Availability: v.Availability,
		Staleness:    v.Staleness,
		OpenRef:      v.OpenRef,
		Claims:       append([]string(nil), v.Claims...),
	}
}
