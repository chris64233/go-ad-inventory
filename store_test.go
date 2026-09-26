package adinventory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileStorePersistAndReopen(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)

	store, err := OpenFileStore(dir, true, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(evCampaignCreated, &Campaign{ID: "c1", TotalBudget: MoneyFromInt(10), DailyBudget: MoneyFromInt(2), CreatedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开并由新服务重放：配置与额度完整恢复。
	store2, err := OpenFileStore(dir, true, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	clock := &mockClock{t: at}
	svc, err := NewService(store2, clock, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.GetCampaign("c1")
	if err != nil {
		t.Fatalf("campaign lost after reopen: %v", err)
	}
	if c.TotalBudget.Cmp(MoneyFromInt(10)) != 0 || c.DailyBudget.Cmp(MoneyFromInt(2)) != 0 {
		t.Fatalf("budget lost: %+v", c)
	}
	if _, err := store2.Append(evReservationFinal, evFinalPayload{Token: "x", Status: StatusCancelled}, at); err != nil {
		t.Fatal(err)
	}
	if seq := store2.LastSeq(); seq != 2 {
		t.Fatalf("last seq after reopen = %d, want 2", seq)
	}
	store2.Close()
}

func TestFileStoreRecoversTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.jsonl")

	store, err := OpenFileStore(dir, false, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	store.Append(evCampaignCreated, &Campaign{ID: "ok"}, time.Now())
	store.Close()

	// 模拟崩溃写了一半的尾行。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":2,"type":"TORN_TAIL_MARKER` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var seen int
	store2, err := OpenFileStore(dir, false, func(ev Event) error {
		seen++
		if ev.Type != evCampaignCreated {
			t.Fatalf("unexpected event: %s", ev.Type)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("open with torn tail: %v", err)
	}
	if seen != 1 {
		t.Fatalf("replayed %d events, want 1 (torn tail must be dropped)", seen)
	}
	// 损坏尾行已被截断，新追加从 seq=2 开始。
	seq, err := store2.Append(evReservationFinal, evFinalPayload{Token: "t", Status: StatusCancelled}, time.Now())
	if err != nil || seq != 2 {
		t.Fatalf("append after recovery: seq=%d err=%v", seq, err)
	}
	store2.Close()

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "TORN_TAIL_MARKER") {
		t.Fatalf("torn line still present:\n%s", data)
	}
}

func TestFileStoreConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenFileStore(dir, false, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.Append(evReservationFinal,
				evFinalPayload{Token: "t", Status: StatusCancelled}, time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	count := 0
	if err := store.Replay(func(Event) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("persisted %d events, want %d", count, n)
	}
}
