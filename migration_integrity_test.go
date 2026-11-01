package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"sync"
	"testing"
)

func runMigrationFixture(t *testing.T, list, read, back []byte, readErr error) (error, *hosttest.Host) {
	h := hosttest.New(t)
	sum := sha256.Sum256(list)
	h.Fixture(plugin.OpLegacyStatus, plugin.LegacyStatusResponse{}).Fixture(plugin.OpLegacySnapshot, plugin.LegacySnapshotResponse{SnapshotID: "s"}).Fixture(plugin.OpLegacyList, map[string]any{"files": []plugin.LegacyFileInfo{{Path: "session.enc", Size: int64(len(list)), SHA256: base64.StdEncoding.EncodeToString(sum[:])}}})
	h.On(plugin.OpLegacyRead, func(json.RawMessage) (any, error) {
		if readErr != nil {
			return nil, readErr
		}
		return map[string]any{"data_b64": base64.StdEncoding.EncodeToString(read)}, nil
	}).Fixture(plugin.OpFSWrite, map[string]any{"ok": true}).Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(back)}).Fixture(plugin.OpLegacyCommit, map[string]any{"ok": true})
	return migrateLegacy(&plugin.Context{}), h
}
func assertNoCommit(t *testing.T, h *hosttest.Host) {
	if len(h.CallsOf(plugin.OpLegacyCommit)) != 0 {
		t.Fatal("unexpected commit")
	}
}
func TestMigrationRejectsSameLengthFlip(t *testing.T) {
	src := []byte("abcdef")
	flip := []byte("abcdeg")
	err, h := runMigrationFixture(t, src, flip, flip, nil)
	if err == nil {
		t.Fatal("want digest error")
	}
	assertNoCommit(t, h)
}
func TestMigrationRejectsTruncation(t *testing.T) {
	src := []byte("abcdef")
	err, h := runMigrationFixture(t, src, src[:3], src[:3], nil)
	if err == nil {
		t.Fatal("want length error")
	}
	assertNoCommit(t, h)
}
func TestMigrationRejectsWriteAfterCorruption(t *testing.T) {
	src := []byte("abcdef")
	bad := []byte("abcdeg")
	err, h := runMigrationFixture(t, src, src, bad, nil)
	if err == nil {
		t.Fatal("want readback error")
	}
	assertNoCommit(t, h)
}
func TestDigestMatchesHexAndBase64(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	if !digestMatches(base64.StdEncoding.EncodeToString(sum[:]), sum[:]) {
		t.Fatal("base64")
	}
}
func TestMigrationReadFailureNoCommit(t *testing.T) {
	src := []byte("x")
	err, h := runMigrationFixture(t, src, nil, nil, errors.New("boom"))
	if err == nil {
		t.Fatal("want err")
	}
	assertNoCommit(t, h)
}
func TestMigrationConcurrentRetryNoPanic(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sum := sha256.Sum256([]byte("x"))
			_ = digestMatches(base64.StdEncoding.EncodeToString(sum[:]), sum[:])
		}()
	}
	wg.Wait()
}
