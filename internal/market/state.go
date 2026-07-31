package market

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const TransactionStateFilename = "transaction-scraper-state.json"

type BackfillState struct {
	Completed                 bool       `json:"completed"`
	Cursor                    string     `json:"cursor,omitempty"`
	UpdatedAt                 time.Time  `json:"updatedAt"`
	AvailableSince            string     `json:"availableSince,omitempty"`
	CompletionReason          string     `json:"completionReason,omitempty"`
	BackfillOldestProcessedAt *time.Time `json:"backfillOldestProcessedAt,omitempty"`
	IncrementalCoveredThrough *time.Time `json:"incrementalCoveredThrough,omitempty"`
}

func StateFile(dataDir string) string {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		dataDir = "./data"
	}
	return filepath.Join(dataDir, TransactionStateFilename)
}

func LoadBackfillState(path string) (BackfillState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return BackfillState{}, nil
		}
		return BackfillState{}, err
	}

	var state BackfillState
	if err := json.Unmarshal(raw, &state); err != nil {
		return BackfillState{}, err
	}
	if state.Completed {
		state.Cursor = ""
	}
	return state, nil
}

func SaveBackfillState(path string, state BackfillState) error {
	if state.Completed {
		state.Cursor = ""
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}
