package main

import "fmt"

// Scorer kinds of the --scorer flag.
const (
	ScorerFake    = "fake"
	ScorerLocalAI = "localai"
	ScorerCLM     = "clm"
)

// Scorer flavours a configuration asks for.
const (
	flavourNone  = ""
	flavourSub1B = "sub1b" // the qualified sub-1B scorer (LocalAI decisions adapter)
	flavourCLM   = "clm"   // the CLM-8B scorer (CLM sidecar adapter)
	flavourAny   = "any"   // whichever scorer --scorer names
)

// ConfigSpec defines one of the five configurations. Rules always run first when the
// configuration has them (the exact rule never asks a model); the scorer runs only on
// what the rules left undecided; the controller runs only where the configuration has
// one (always for 4, only after a scorer fallback for 5). A decision nothing decided ends
// in clarify: nothing is executed.
type ConfigSpec struct {
	ID         int
	Name       string
	Rules      bool
	Flavour    string
	Controller bool
}

// Configs are the five configurations, in order.
var Configs = []ConfigSpec{
	{ID: 1, Name: "deterministic-rules-only", Rules: true},
	{ID: 2, Name: "qualified-sub-1b-scorer", Rules: true, Flavour: flavourSub1B},
	{ID: 3, Name: "clm-8b-scorer", Rules: true, Flavour: flavourCLM},
	{ID: 4, Name: "direct-controller-routing", Controller: true},
	{ID: 5, Name: "scorer-then-controller", Rules: true, Flavour: flavourAny, Controller: true},
}

// SelectConfigs resolves the --config value for a scorer kind. "all" runs every
// configuration the scorer can serve and reports the skipped ones in skipped; a number
// names exactly one and is a usage error when the scorer cannot serve it.
func SelectConfigs(arg, scorer string) (run []ConfigSpec, skipped []string, err error) {
	serves := func(c ConfigSpec) bool {
		switch c.Flavour {
		case flavourSub1B:
			return scorer == ScorerFake || scorer == ScorerLocalAI
		case flavourCLM:
			return scorer == ScorerFake || scorer == ScorerCLM
		}
		return true
	}
	if arg == "all" {
		for _, c := range Configs {
			if serves(c) {
				run = append(run, c)
			} else {
				skipped = append(skipped, fmt.Sprintf("configuration %d (%s) needs the %s scorer", c.ID, c.Name, c.Flavour))
			}
		}
		return run, skipped, nil
	}
	var id int
	if _, err := fmt.Sscanf(arg, "%d", &id); err != nil || fmt.Sprint(id) != arg || id < 1 || id > len(Configs) {
		return nil, nil, fmt.Errorf("--config must be all or 1 to %d, got %q", len(Configs), arg)
	}
	c := Configs[id-1]
	if !serves(c) {
		return nil, nil, fmt.Errorf("configuration %d (%s) needs the %s scorer; --scorer %s cannot serve it", c.ID, c.Name, c.Flavour, scorer)
	}
	return []ConfigSpec{c}, nil, nil
}

// flavourFor returns the scorer flavour configuration c uses under scorer kind scorer.
func flavourFor(c ConfigSpec, scorer string) string {
	if c.Flavour == flavourAny {
		switch scorer {
		case ScorerCLM:
			return flavourCLM
		default:
			return flavourSub1B
		}
	}
	return c.Flavour
}
