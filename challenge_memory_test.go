package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChallengeMemoryTTLAndIsolation(t *testing.T) {
	m := newChallengeMemory()
	now := time.Unix(1, 0)
	m.now = func() time.Time { return now }
	m.put("a", "ua", "pa")
	m.put("b", "ub", "pb")
	a, _ := m.get("a")
	b, _ := m.get("b")
	if a.password != "pa" || b.password != "pb" {
		t.Fatal("cross account")
	}
	now = now.Add(challengeTTL + time.Second)
	if _, err := m.get("a"); err == nil {
		t.Fatal("expired secret survived")
	}
}
func TestRestartRequiresPasswordAgain(t *testing.T) {
	m := newChallengeMemory()
	m.put("s", "u", "p")
	m = newChallengeMemory()
	if _, err := m.get("s"); err == nil || !strings.Contains(err.Error(), "重新输入密码") {
		t.Fatal(err)
	}
}
func TestChallengeSecretNeverSerializes(t *testing.T) {
	s := loginSession{State: "need_challenge", Protocol: protocolState{Challenge: &loginChallenge{Kind: "captcha"}}}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "secret-password") {
		t.Fatal(string(raw))
	}
	typ := string(raw)
	if strings.Contains(strings.ToLower(typ), "password") {
		t.Fatal(typ)
	}
}
func TestLocationDoesNotImplyCompleted(t *testing.T) {
	s := loginSession{}
	r := authResponse{Location: "https://sts.api.io.mi.com/sts"}
	if r.Location != "" {
		s.State = "need_token_exchange"
	}
	if s.State == "completed" {
		t.Fatal("location bypassed token gate")
	}
}
func TestChallengeClearOnDisable(t *testing.T) {
	m := newChallengeMemory()
	m.put("a", "u", "p")
	m.clear()
	if _, err := m.get("a"); err == nil {
		t.Fatal("secret survived clear")
	}
}
