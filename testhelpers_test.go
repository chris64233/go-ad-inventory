package adinventory

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// errInjected 用于模拟 WAL 写入失败。
var errInjected = errors.New("injected store failure")

// memStore 是测试专用的内存 Store，行为与 FileStore 一致（串行追加）。
type memStore struct {
	mu     sync.Mutex
	events []Event
	fail   bool
}

func (m *memStore) Append(typ string, payload any, at time.Time) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return 0, errInjected
	}
	m.events = append(m.events, Event{Seq: int64(len(m.events) + 1), Type: typ, At: at, Payload: raw})
	return int64(len(m.events)), nil
}

func (m *memStore) Replay(fn func(Event) error) error {
	m.mu.Lock()
	evs := append([]Event(nil), m.events...)
	m.mu.Unlock()
	for _, ev := range evs {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

func (m *memStore) Close() error { return nil }

func (m *memStore) eventTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.events))
	for i, ev := range m.events {
		out[i] = ev.Type
	}
	return out
}

func mustMoney(t *testing.T, s string) Money {
	t.Helper()
	m, err := ParseMoney(s)
	if err != nil {
		t.Fatalf("ParseMoney(%q): %v", s, err)
	}
	return m
}

// newTestService 构造使用固定时钟的服务，时钟起始于一个已知 UTC 时刻。
func newTestService(t *testing.T, ttl time.Duration) (*Service, *memStore, *mockClock) {
	t.Helper()
	store := &memStore{}
	clock := &mockClock{t: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)}
	svc, err := NewService(store, clock, ttl)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store, clock
}

func seedCampaign(t *testing.T, svc *Service, id, total, daily string) {
	t.Helper()
	if _, err := svc.CreateCampaign(id, mustMoney(t, total), mustMoney(t, daily)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
}

func assertErrKind(t *testing.T, err error, want ErrorKind) {
	t.Helper()
	be, ok := AsError(err)
	if !ok {
		t.Fatalf("want *Error kind %s, got %v", want, err)
	}
	if be.Kind != want {
		t.Fatalf("error kind = %s, want %s (msg: %s)", be.Kind, want, be.Msg)
	}
}
