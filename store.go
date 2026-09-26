package adinventory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event 是 WAL 中一条不可变的事实记录。服务的全部状态都可以通过
// 按序重放 Event 得到，因此任何额度变化落盘后即便进程崩溃也不会丢失。
type Event struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

// 以下常量是各 Event.Type 的取值。
const (
	evCampaignCreated    = "campaign_created"
	evReservationCreated = "reservation_created"
	evReservationFinal   = "reservation_finalized"
)

// evFinalPayload 是凭证进入终态（核销/取消/过期）事件的载荷。
type evFinalPayload struct {
	Token      string            `json:"token"`
	Status     ReservationStatus `json:"status"`
	ReceiptNo  string            `json:"receipt_no,omitempty"`
	ActualCost Money             `json:"actual_cost"`
}

// Store 是事件追加与重放接口。
type Store interface {
	// Append 以原子写入持久化一条事件，返回事件序号。
	Append(typ string, payload any, at time.Time) (int64, error)
	// Replay 按写入顺序把全部事件交给 fn 处理。
	Replay(fn func(Event) error) error
	// Close 关闭底层文件。
	Close() error
}

// FileStore 是基于 append-only JSONL 文件的 Store 实现。
// 每条事件单行 JSON 写入并 fsync，崩溃最多丢失未落盘的最后半行
// （重放时跳过损坏尾行），已确认的事件不会丢。
type FileStore struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	w      *bufio.Writer
	seq    int64
	fsync  bool
	closed bool
}

// OpenFileStore 在 dir 下打开（不存在则创建）WAL 文件并重放，
// apply 用于把历史事件装载回内存状态。
func OpenFileStore(dir string, fsync bool, apply func(Event) error) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("adinventory: empty store dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("adinventory: mkdir store: %w", err)
	}
	path := filepath.Join(dir, "wal.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("adinventory: open wal: %w", err)
	}
	s := &FileStore{path: path, f: f, w: bufio.NewWriter(f), fsync: fsync}
	if err := s.recoverAndReplay(apply); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// LastSeq 返回已持久化的最大事件序号。
func (s *FileStore) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *FileStore) recoverAndReplay(apply func(Event) error) error {
	// 逐行扫描；最后一行若损坏（崩溃写了一半）则截断它。
	data, err := io.ReadAll(s.f)
	if err != nil {
		return fmt.Errorf("adinventory: read wal: %w", err)
	}
	var validLen int64
	seq := int64(0)
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			break
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// 遇到损坏尾行：截断到最后一个有效位置。
			if terr := s.f.Truncate(validLen); terr != nil {
				return fmt.Errorf("adinventory: truncate torn wal: %w", terr)
			}
			break
		}
		if err := apply(ev); err != nil {
			return fmt.Errorf("adinventory: replay event %d: %w", ev.Seq, err)
		}
		if ev.Seq > seq {
			seq = ev.Seq
		}
		validLen += int64(len(line)) + 1
	}
	s.seq = seq
	return nil
}

// Append 实现 Store。
func (s *FileStore) Append(typ string, payload any, at time.Time) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("adinventory: encode event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("adinventory: store closed")
	}
	s.seq++
	ev := Event{Seq: s.seq, Type: typ, At: at.UTC(), Payload: raw}
	line, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("adinventory: encode event: %w", err)
	}
	line = append(line, '\n')
	if _, err := s.w.Write(line); err != nil {
		return 0, fmt.Errorf("adinventory: write wal: %w", err)
	}
	if err := s.w.Flush(); err != nil {
		return 0, fmt.Errorf("adinventory: flush wal: %w", err)
	}
	if s.fsync {
		if err := s.f.Sync(); err != nil {
			return 0, fmt.Errorf("adinventory: fsync wal: %w", err)
		}
	}
	return ev.Seq, nil
}

// Replay 实现 Store。
func (s *FileStore) Replay(fn func(Event) error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("adinventory: store closed")
	}
	path := s.path
	s.mu.Unlock()

	// 从磁盘文件独立读取，避免与追加位置互相干扰。
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("adinventory: open wal for replay: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("adinventory: decode wal line: %w", err)
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Close 实现 Store。
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.w.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	return s.f.Close()
}
