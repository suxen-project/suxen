package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBlobStoreValidationAndConfigurationRedaction(t *testing.T) {
	valid := BlobStore{
		Name:   "archive",
		Driver: "s3",
		ConfigurationRef: &ConfigurationReference{
			Env: "SUXEN_ARCHIVE",
		},
		PhysicalIdentity: strings.Repeat("a", 64),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "access") {
		t.Fatalf("serialized blob store leaked configuration: %s", encoded)
	}

	invalid := valid
	invalid.ConfigurationRef = &ConfigurationReference{
		Env:  "SUXEN_ARCHIVE",
		File: "/run/secrets/archive",
	}
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidBlobStoreConfig) {
		t.Fatalf("Validate() error = %v", err)
	}
}
