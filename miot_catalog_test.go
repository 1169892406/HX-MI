package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"testing"
)

func TestMIoTOfficialSnapshotDeterministicAndAuditable(t *testing.T) {
	rows, err := loadMIoTInstanceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[[4]string]bool{}
	for i, r := range rows {
		if r.Model == "" || r.Type == "" || r.Status == "" || r.Version < 1 {
			t.Fatalf("row %d invalid: %+v", i, r)
		}
		key := [4]string{r.Model, r.Type, r.Status, strconv.Itoa(r.Version)}
		if seen[key] {
			t.Fatalf("duplicate tuple: %+v", r)
		}
		seen[key] = true
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Status < b.Status
	}) {
		t.Fatal("snapshot is not deterministically sorted")
	}
	var meta struct {
		SnapshotSHA256 string `json:"snapshot_sha256"`
		UniqueCount    int    `json:"unique_count"`
		SourceURL      string `json:"source_url"`
	}
	if err := json.Unmarshal(miotInstancesMetadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.UniqueCount != len(rows) {
		t.Fatalf("metadata count=%d rows=%d", meta.UniqueCount, len(rows))
	}
	zr, err := gzip.NewReader(bytes.NewReader(miotInstancesSnapshotGz))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	rawSnapshot, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rawSnapshot)
	if hex.EncodeToString(sum[:]) != meta.SnapshotSHA256 {
		t.Fatal("snapshot hash mismatch")
	}
	if meta.SourceURL != "https://miot-spec.org/miot-spec-v2/instances?status=all" {
		t.Fatal("unexpected source URL")
	}
	if _, err := os.Stat("miotcatalog/instances.raw.json"); err != nil {
		t.Fatal(err)
	}
}
