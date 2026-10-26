package mistral

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{CredentialRef: "cred_1", Model: "mistral-small-latest"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if c := ok.compat(); c.BaseURL != DefaultBaseURL {
		t.Fatalf("%+v", c)
	}
}

func TestConfigStringHoldsNoSecret(t *testing.T) {
	s := (Config{CredentialRef: "cred_SECRET", Model: "mistral-small-latest"}).String()
	if strings.Contains(s, "cred_") {
		t.Fatal(s)
	}
}
