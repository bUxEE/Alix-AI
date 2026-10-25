package providersuite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Mode says how a run was made.
const (
	ModeFixtures = "fixtures"
	ModeLive     = "live"
)

// DeclaredMedia is what a profile declares for the three conditional
// multimodal checks. It is the registry's rule (providerreg declaredMedia): a
// modality counts as declared when it is an input or an output, so a report
// made here is not refused by RecordConformance as a declaration mismatch.
func DeclaredMedia(p pv.ProviderProfile) (image, file, audio bool) {
	has := func(m pv.Modality) bool {
		for _, x := range p.Modalities.Input {
			if x == m {
				return true
			}
		}
		for _, x := range p.Modalities.Output {
			if x == m {
				return true
			}
		}
		return false
	}
	return has(pv.ModalityImage), has(pv.ModalityFile), has(pv.ModalityAudio)
}

var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ReportInput is what a report is made from.
type ReportInput struct {
	Rows    []Row
	Profile pv.ProviderProfile
	// Mode is ModeFixtures or ModeLive.
	Mode               string
	Started, Completed time.Time
	// Clock times the report id (nil: wall clock).
	Clock clock.Clock
}

// BuildReport assembles the conformance report of a run, validates it against
// schemas/providers/conformance-report and decodes it into the typed report
// the registry records (providerreg RecordConformance). It returns the JSON
// form too. An invalid report is an error: nothing is returned to record.
func BuildReport(in ReportInput) (pv.ConformanceReport, []byte, error) {
	if in.Mode != ModeFixtures && in.Mode != ModeLive {
		return pv.ConformanceReport{}, nil, fmt.Errorf("providersuite: unknown mode %q", in.Mode)
	}
	groups := map[string]map[string]any{}
	failed := false
	for _, r := range in.Rows {
		if groups[r.Group] == nil {
			groups[r.Group] = map[string]any{}
		}
		var reason any
		if r.Result == Failed {
			failed = true
			code := r.Reason
			if !reasonRE.MatchString(code) {
				code = "check_failed"
			}
			reason = code
		}
		groups[r.Group][r.Name] = map[string]any{"result": string(r.Result), "reason": reason}
	}
	image, file, audio := DeclaredMedia(in.Profile)
	outcome := "passed"
	if failed {
		outcome = "failed"
	}
	g := ids.NewGenerator(in.Clock)
	stamp := func(t time.Time) string { return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z") }
	doc := map[string]any{
		"report_id": g.New("conf"),
		"provider": map[string]any{
			"profile_id": string(in.Profile.ProfileID), "provider_id": string(in.Profile.ProviderID),
			"model_id": string(in.Profile.ModelID), "model_version": string(in.Profile.ModelVersion),
		},
		"suite":        map[string]any{"version": SuiteVersion, "mode": in.Mode},
		"declared":     map[string]any{"image": image, "file": file, "audio": audio},
		"started_at":   stamp(in.Started),
		"completed_at": stamp(in.Completed),
		"outcome":      outcome,
		"groups":       groups,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return pv.ConformanceReport{}, nil, err
	}
	if err := ValidateAgainst(SchemaConformanceReport, raw); err != nil {
		return pv.ConformanceReport{}, nil, err
	}
	var rep pv.ConformanceReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return pv.ConformanceReport{}, nil, fmt.Errorf("providersuite: the report does not decode into the typed report: %w", err)
	}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return pv.ConformanceReport{}, nil, err
	}
	if err := ValidateAgainst(SchemaConformanceReport, out); err != nil {
		return pv.ConformanceReport{}, nil, err
	}
	return rep, append(out, '\n'), nil
}
