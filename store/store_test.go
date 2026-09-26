package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}

	batch1 := []Event{
		{Type: "a", At: time.Now(), Data: json.RawMessage(`{"n":1}`)},
		{Type: "b", At: time.Now(), Data: json.RawMessage(`{"n":2}`)},
	}
	if err := st.Append(context.Background(), batch1); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), []Event{
		{Type: "c", At: time.Now(), Data: json.RawMessage(`{"n":3}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：序号连续，事件完整。
	st2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	events, err := st2.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d", i, ev.Seq)
		}
	}
	if events[2].Type != "c" {
		t.Fatalf("last event type = %s", events[2].Type)
	}

	// 追加新事件，序号应接着 3。
	if err := st2.Append(context.Background(), []Event{
		{Type: "d", At: time.Now(), Data: json.RawMessage(`{"n":4}`)},
	}); err != nil {
		t.Fatal(err)
	}
	events, _ = st2.Events(context.Background())
	if len(events) != 4 || events[3].Seq != 4 {
		t.Fatalf("after reopen append: len=%d seq=%d", len(events), events[3].Seq)
	}
}

func TestFileStoreSkipsCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	st, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), []Event{
		{Type: "a", At: time.Now(), Data: json.RawMessage(`{"n":1}`)},
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 模拟崩溃留下的残行。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"broken"`)
	f.Close()

	st2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	events, err := st2.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (corrupt tail skipped)", len(events))
	}

	// 残行之后追加的新事件必须自成一行并正常读出。
	if err := st2.Append(context.Background(), []Event{
		{Type: "after", At: time.Now(), Data: json.RawMessage(`{"n":2}`)},
	}); err != nil {
		t.Fatal(err)
	}
	st2.Close()
	st3, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	events, err = st3.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != "after" || events[1].Seq != 2 {
		t.Fatalf("after corrupt tail: %+v", events)
	}
}

func TestMemStoreClosed(t *testing.T) {
	st := NewMemStore()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), []Event{{Type: "x"}}); err != ErrClosed {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
