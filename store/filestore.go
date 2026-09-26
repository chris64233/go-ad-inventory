package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// ErrClosed 在存储关闭后继续写入时返回。
var ErrClosed = errors.New("store: closed")

// FileStore 将事件以 JSON Lines 形式只追加落盘。
//
// 每批事件先写入 bufio 缓冲，Flush 后对文件做一次 Fsync；
// 一批事件在进程内互斥区中顺序写出，因而对外部呈现原子效果，
// 崩溃时最坏情况是最后一批未 Sync 的行丢失，加载时残行被跳过。
type FileStore struct {
	mu           sync.Mutex
	f            *os.File
	path         string
	nextSeq      int64
	needsNewline bool // 文件末尾是否缺少 '\n'（崩溃残行），追加前需先补换行
}

// NewFileStore 打开（不存在则创建）path 指向的事件日志，
// 并根据已有完整行确定下一个事件序号。
func NewFileStore(path string) (*FileStore, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	count, needsNL, err := inspectLog(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &FileStore{f: f, path: path, nextSeq: count + 1, needsNewline: needsNL}, nil
}

// Append 将整批事件顺序写入并 Fsync，分配连续序号。
func (s *FileStore) Append(_ context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return ErrClosed
	}
	bw := bufio.NewWriter(s.f)
	if s.needsNewline {
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
		s.needsNewline = false
	}
	for i := range events {
		events[i].Seq = s.nextSeq + int64(i)
		line, err := json.Marshal(events[i])
		if err != nil {
			return err
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.nextSeq += int64(len(events))
	return nil
}

// Events 从磁盘读取全部完整事件行；末尾不完整的残行会被忽略。
func (s *FileStore) Events(_ context.Context) ([]Event, error) {
	s.mu.Lock()
	path := s.path
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Event
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev Event
			if jerr := json.Unmarshal(line, &ev); jerr == nil {
				out = append(out, ev)
			}
			// 残行（损坏 JSON / 未写完）直接丢弃。
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Close 关闭底层文件。
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// inspectLog 统计事件日志中以 '\n' 结尾的完整行数，并报告文件是否以
// 不含 '\n' 的非空残行结尾（崩溃时未写完的一行）。
func inspectLog(path string) (count int64, needsNewline bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	buf := make([]byte, 32*1024)
	var trailing byte
	for {
		n, rerr := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				count++
			}
		}
		if n > 0 {
			trailing = buf[n-1]
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return 0, false, rerr
		}
	}
	if trailing != 0 && trailing != '\n' {
		needsNewline = true
	}
	return count, needsNewline, nil
}
