package openrouter

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{CredentialRef: "cred_1", Model: "openai/gpt-4o-mini"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if c := ok.compat(); c.BaseURL != DefaultBaseURL || c.APIPrefix != "/api/v1" {
		t.Fatalf("%+v", c)
	}
	c := ok
	c.CredentialRef = ""
	if err := c.Validate(); err == nil {
		t.Fatal("accepted empty credential ref")
	}
}

func TestConfigStringHoldsNoSecret(t *testing.T) {
	s := (Config{CredentialRef: "cred_SECRET", Model: "x/y"}).String()
	if strings.Contains(s, "cred_") {
		t.Fatal(s)
	}
}
