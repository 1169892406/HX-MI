package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestProductionManifestMigrationSecretContract(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		SchemaVersion int      `json:"schema_version"`
		APIVersion    string   `json:"api_version"`
		Permissions   []string `json:"permissions"`
		Resources     struct {
			Migrations []struct {
				Name       string   `json:"name"`
				SecretKeys []string `json:"secret_keys"`
			} `json:"migrations"`
			Secrets struct {
				Blobs []struct {
					Key, Scope string
					Rotatable  bool `json:"rotatable"`
				} `json:"blobs"`
			} `json:"secrets"`
		} `json:"resources"`
	}
	if json.Unmarshal(raw, &m) != nil {
		t.Fatal("manifest")
	}
	if m.SchemaVersion != 2 || m.APIVersion != "2" {
		t.Fatalf("schema/api=%d/%s", m.SchemaVersion, m.APIVersion)
	}
	hasPerm := false
	for _, p := range m.Permissions {
		hasPerm = hasPerm || p == "migration.device_secret"
	}
	if !hasPerm {
		t.Fatal("missing permission")
	}
	if len(m.Resources.Migrations) != 1 || m.Resources.Migrations[0].Name != "hx_mi_legacy" || len(m.Resources.Migrations[0].SecretKeys) != 1 || m.Resources.Migrations[0].SecretKeys[0] != "miio_token" {
		t.Fatalf("migration=%+v", m.Resources.Migrations)
	}
	found := false
	for _, b := range m.Resources.Secrets.Blobs {
		if b.Key == "miio_token" {
			found = b.Scope == "device" && b.Rotatable
		}
	}
	if !found {
		t.Fatal("miio_token blob contract")
	}
}
