package mqtt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
)

// topicsFile is the name of the file holding the subscription set of an
// input, under data_dir/inputs/{input_id} (decision Q7).
const topicsFile = "mqtt-topics.json"

// errNoDataDir is the reason the subscription set is not kept: without a
// data directory the input never unsubscribes a topic it no longer uses.
var errNoDataDir = errors.New("mqtt: data_dir unknown, the subscription set is not kept")

// topicsDir is the directory of the subscription set of input id, the one
// the transform state of the input already uses.
func topicsDir(dataDir, id string) string {
	return filepath.Join(dataDir, "inputs", id)
}

// readTopics returns the subscription set the input last wrote. A missing
// or unreadable file is an error the caller logs: there is then nothing to
// unsubscribe.
func readTopics(dataDir, id string) ([]string, error) {
	if dataDir == "" {
		return nil, errNoDataDir
	}

	data, err := os.ReadFile(filepath.Join(topicsDir(dataDir, id), topicsFile))
	if err != nil {
		return nil, err
	}

	var topics []string
	if err := json.Unmarshal(data, &topics); err != nil {
		return nil, err
	}

	return topics, nil
}

// writeTopics replaces the subscription set of the input with topics, a
// sorted list without duplicates.
func writeTopics(dataDir, id string, topics []string) error {
	if dataDir == "" {
		return errNoDataDir
	}

	dir := topicsDir(dataDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(topics)
	if err != nil {
		return err
	}

	return atomicfile.Write(dir, topicsFile, data)
}

// staleTopics returns the topics of old that are not in current, sorted
// and without duplicates.
func staleTopics(old, current []string) []string {
	var stale []string
	for _, topic := range old {
		if !slices.Contains(current, topic) && !slices.Contains(stale, topic) {
			stale = append(stale, topic)
		}
	}
	slices.Sort(stale)

	return stale
}
