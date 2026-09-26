// Package store 提供预算领域的事件持久化。
//
// 所有状态与额度变化都以只追加（append-only）事件的形式保存，
// 内存态由事件重放得到，杜绝“状态改了但额度流水丢了”的情况。
package store

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Event 是持久化事件信封。Data 为具体事件负载的原始 JSON。
type Event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// Store 是事件存储抽象。Append 必须保证整批事件原子写入：
// 要么全部带连续序号落盘，要么全部不写入。
type Store interface {
	Append(ctx context.Context, events []Event) error
	Events(ctx context.Context) ([]Event, error)
	// Close 释放底层资源。
	Close() error
}

// MemStore 是纯内存事件存储，主要用于测试。
type MemStore struct {
	mu     sync.Mutex
	events []Event
	closed bool
}

// NewMemStore 创建空的内存事件存储。
func NewMemStore() *MemStore { return &MemStore{} }

// Append 原子地追加一批事件并分配连续序号。
func (s *MemStore) Append(_ context.Context, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if len(events) == 0 {
		return nil
	}
	next := int64(len(s.events)) + 1
	for i := range events {
		events[i].Seq = next + int64(i)
	}
	s.events = append(s.events, events...)
	return nil
}

// Events 返回已持久化事件的拷贝。
func (s *MemStore) Events(_ context.Context) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

// Close 标记存储关闭。
func (s *MemStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
