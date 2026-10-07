package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"inf-backup/internal/protocol"
)

func TestPythonJournalMigrationAndBoundedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Matches the v2 Python journal's JSON topology, including null current.
	legacy := `{"current":{"protocol":1,"request_id":"old-request","operation":"backup","event":"accepted","payload":{"message":"task accepted"}},"history":[{"protocol":1,"request_id":"old-dry-run","operation":"dry-run","event":"succeeded","payload":{"message_type":"summary","dry_run":true}}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Current != nil || s.Find("old-request").Event != "interrupted" || s.Find("old-request").Payload["uncertain"] != true {
		t.Fatal("active Python request was not recovered as uncertain")
	}
	if s.Find("old-dry-run").Payload["dry_run"] != true {
		t.Fatal("Python terminal state lost")
	}
	for i := 0; i < 40; i++ {
		if err := s.Finish(protocol.New(fmt.Sprintf("request-%d", i), "check", "succeeded", map[string]any{"num_errors": 0})); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.History) != HistoryLimit || reopened.Find("request-0") != nil || reopened.Find("request-39") == nil {
		t.Fatal("history bound or durable terminal result lost")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("journal is not private")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatal("temporary journals leaked")
	}
}

func TestBeginFailureDoesNotKeepAcceptedRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: path, History: []protocol.Event{}}
	if err := s.Begin(protocol.New("request", "backup", "accepted", nil)); err == nil || s.Current != nil {
		t.Fatal("failed acceptance retained a current request")
	}
}

func TestCorruptJournalBlocksStartup(t *testing.T) {
	for _, content := range []string{"not-json", "null", `{"current":null,"history":null}`, `{"current":{"request_id":"request"},"history":[]}`} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("invalid journal allowed new work")
			}
			data, _ := os.ReadFile(path)
			if string(data) != content {
				t.Fatal("corrupt recovery evidence overwritten")
			}
		})
	}
}

func TestRestartPersistsInterruptionOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	if err := s.Begin(protocol.New("preview", "dry-run", "accepted", nil)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil || len(s.History) != 1 || s.History[0].Operation != "dry-run" {
		t.Fatal("restarting duplicated or lost the recovered request")
	}
	data, _ := os.ReadFile(path)
	var journal map[string]any
	if err = json.Unmarshal(data, &journal); err != nil || journal["current"] != nil {
		t.Fatal("active request not cleared on disk")
	}
}
