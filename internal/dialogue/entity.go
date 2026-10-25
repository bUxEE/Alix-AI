package dialogue

import (
	"context"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Entity resolution (pipeline step 8): a mention the user made of an application
// record ("The Leopard") becomes the record's stable ID and the data version the
// resolution relied on. The host application does it, through an adapter Read;
// the model never does. An ambiguous mention becomes a clarification that lists the
// host's candidates, and a mention that matches nothing is refused, never guessed.

// ResolveRequest asks the host to resolve one mention.
type ResolveRequest struct {
	// CapabilityID is the capability whose field the mention fills. The policy
	// gateway has already allowed it for this caller.
	CapabilityID string
	// Field is the capability field the mention fills.
	Field string
	// EntityType is the application's entity type of the field.
	EntityType string
	// Mention is the user's words for the entity (or, for a field with no resolver,
	// the ID the user gave). It is untrusted data.
	Mention string
	// CandidateID is an ID the model proposed, if any: a hint the host verifies.
	CandidateID string
	// Limit is the most candidates the engine will use.
	Limit int
}

// EntityCandidate is one record the host found.
type EntityCandidate struct {
	// ID is the record's stable ID in the application.
	ID string
	// Label is the host's display text for it; empty when the host gave none.
	Label string
}

// Resolution is the host's answer.
type Resolution struct {
	Candidates []EntityCandidate
	// Version is the data version the resolution relied on (the host's snapshot
	// version), recorded with the resolved entity.
	Version string
}

// EntityResolver resolves mentions through the host application. The default is
// AdapterResolver.
type EntityResolver interface {
	Resolve(ctx context.Context, scope contracts.Scope, req ResolveRequest) (Resolution, error)
}

// maxResolveCandidates bounds how many candidates a resolution may return.
const maxResolveCandidates = 20

// labelField is the name of the text fact that carries a candidate's display
// label in a resolution result: the i-th "label" value belongs to the i-th
// identifier.
const labelField = "label"

// AdapterResolver returns the default EntityResolver: an adapter Read.
//
// The package schema lets an entity type declare a resolver operation
// (entity_types.<type>.resolve), and the AppAdapter contract has no operation of
// its own for it, so a resolution is a Read of the capability that declares the
// field, with the reserved arguments entity_type and mention (and candidate_id
// when the model proposed one). The adapter maps that to the declared resolver; the
// application reauthorises every resolved entity. The reply is an ordinary
// DeterministicResult: identifiers are the candidates (record references of the
// entity type), a text value named "label" at the same position is the candidate's
// display text, and the freshness version is the data version of the resolution.
// A rejection of the read that is a plain validation failure means no match; any
// other rejection is the host refusing and is typed as the host's code.
func AdapterResolver(src actions.AdapterSource) EntityResolver { return adapterResolver{src: src} }

type adapterResolver struct{ src actions.AdapterSource }

func (r adapterResolver) Resolve(ctx context.Context, scope contracts.Scope, req ResolveRequest) (Resolution, error) {
	ad, err := r.src.Adapter(ctx, scope.ApplicationID)
	if err != nil {
		return Resolution{}, err
	}
	fields := map[string]any{"entity_type": req.EntityType, "mention": req.Mention}
	if req.CandidateID != "" {
		fields["candidate_id"] = req.CandidateID
	}
	res, err := ad.Read(ctx, scope, adapters.ReadRequest{CapabilityID: req.CapabilityID, Fields: fields})
	if err != nil {
		return Resolution{}, err
	}
	switch res.Status {
	case act.DeterministicResultStatusOk:
	case act.DeterministicResultStatusRejected:
		if len(res.Errors) > 0 && contracts.ErrCode(res.Errors[0].Code) != contracts.ErrValidationFailed {
			return Resolution{}, contracts.New(contracts.ErrCode(res.Errors[0].Code), "the application refused to resolve the reference")
		}
		return Resolution{}, nil // no match
	case act.DeterministicResultStatusUnavailable:
		return Resolution{}, contracts.New(contracts.ErrUnavailable, "the application could not resolve the reference")
	default:
		return Resolution{}, contracts.New(contracts.ErrSchemaInvalid, "the application answered a resolution with an unexpected status")
	}
	var labels []string
	for _, v := range res.Values {
		if string(v.Field) == labelField {
			labels = append(labels, canonicalText(v.Value))
		}
	}
	out := Resolution{}
	if res.Freshness != nil {
		out.Version = string(res.Freshness.Version)
	}
	seen := map[string]bool{}
	for _, rec := range res.Identifiers {
		if string(rec.EntityType) != req.EntityType || seen[string(rec.EntityID)] {
			continue
		}
		seen[string(rec.EntityID)] = true
		c := EntityCandidate{ID: string(rec.EntityID)}
		if len(labels) == len(res.Identifiers) {
			// Labels pair with identifiers by position only when every identifier has one.
			for i, id := range res.Identifiers {
				if id.EntityID == rec.EntityID && id.EntityType == rec.EntityType {
					c.Label = labels[i]
					break
				}
			}
		}
		out.Candidates = append(out.Candidates, c)
		if len(out.Candidates) == maxResolveCandidates {
			break
		}
	}
	return out, nil
}
