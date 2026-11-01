package main

import (
	"errors"
	"sync"
	"time"
)

const challengeTTL = 5 * time.Minute

type challengeSecret struct {
	password string
	user     string
	expires  time.Time
}
type challengeMemory struct {
	mu     sync.Mutex
	values map[string]challengeSecret
	now    func() time.Time
}

func newChallengeMemory() *challengeMemory {
	return &challengeMemory{values: map[string]challengeSecret{}, now: time.Now}
}
func (m *challengeMemory) put(id, user, password string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	m.values[id] = challengeSecret{password: password, user: user, expires: m.now().Add(challengeTTL)}
}
func (m *challengeMemory) get(id string) (challengeSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	v, ok := m.values[id]
	if !ok {
		return v, errors.New("挑战已过期或服务已重启，请重新输入密码")
	}
	return v, nil
}
func (m *challengeMemory) delete(id string) { m.mu.Lock(); delete(m.values, id); m.mu.Unlock() }
func (m *challengeMemory) clear() {
	m.mu.Lock()
	for k := range m.values {
		delete(m.values, k)
	}
	m.mu.Unlock()
}
func (m *challengeMemory) gcLocked() {
	now := m.now()
	for k, v := range m.values {
		if !v.expires.After(now) {
			delete(m.values, k)
		}
	}
}

var challengeSecrets = newChallengeMemory()
