// Package state keeps the Python v2 journal format for in-place upgrades.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"inf-backup/internal/protocol"
)

const HistoryLimit = 32

type Store struct {
	Path    string           `json:"-"`
	Current *protocol.Event  `json:"current"`
	History []protocol.Event `json:"history"`
}

func Open(path string) (*Store, error) {
	s := &Store{Path: path, History: []protocol.Event{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, errors.New("无法读取任务状态文件；请检查权限")
	}
	var topology map[string]json.RawMessage
	if json.Unmarshal(data, &topology) != nil || topology == nil || topology["current"] == nil || topology["history"] == nil || json.Unmarshal(data, s) != nil || s.History == nil {
		return nil, errors.New("任务状态文件无效；请保留该文件并检查快照，勿自动重试备份")
	}
	valid := func(e protocol.Event, active bool) bool {
		return e.Protocol == protocol.Version && protocol.ValidID(e.ID()) && protocol.ValidOperation(e.Operation) && e.Payload != nil &&
			((active && (e.Event == "accepted" || e.Event == "progress")) || (!active && protocol.Terminal(e.Event)))
	}
	for _, record := range s.History {
		if !valid(record, false) {
			return nil, errors.New("任务历史记录无效；请保留日志并检查快照")
		}
	}
	if len(s.History) > HistoryLimit {
		s.History = s.History[len(s.History)-HistoryLimit:]
	}
	if s.Current != nil {
		if !valid(*s.Current, true) {
			return nil, errors.New("活动任务记录无效；请保留日志并检查快照")
		}
		record := protocol.New(s.Current.ID(), s.Current.Operation, "interrupted", map[string]any{
			"kind": "wrapper_restarted", "message": "wrapper restarted while the operation was active", "uncertain": true,
		})
		if err = s.Finish(record); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Save() error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".inf-backup-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.Path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) Begin(record protocol.Event) error {
	previous := s.Current
	s.Current = &record
	if err := s.Save(); err != nil {
		s.Current = previous
		return err
	}
	return nil
}

func (s *Store) Finish(record protocol.Event) error {
	s.Current = nil
	s.History = append(s.History, record)
	if len(s.History) > HistoryLimit {
		s.History = s.History[len(s.History)-HistoryLimit:]
	}
	return s.Save()
}

func (s *Store) Find(id string) *protocol.Event {
	if s.Current != nil && s.Current.ID() == id {
		return s.Current
	}
	for i := len(s.History) - 1; i >= 0; i-- {
		if s.History[i].ID() == id {
			return &s.History[i]
		}
	}
	return nil
}
