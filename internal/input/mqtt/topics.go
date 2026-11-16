package mqtt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
)

// The subscription set of an input is data_dir/mqtt/topics/{input_id}.json
// (decision Q7, amended by the final review). It stays outside
// data_dir/inputs/{input_id}, which the supervisor deletes on a rollback and
// when an input leaves the document: the set must survive both, or a topic
// subscribed by a document that was rolled back would stay in the persistent
// session with no record of it.
const (
	topicsSubdir    = "mqtt/topics"
	topicsExtension = ".json"
)

// errNoDataDir is the reason the subscription set is not kept: without a
// data directory the input never unsubscribes a topic it no longer uses.
var errNoDataDir = errors.New("mqtt: data_dir unknown, the subscription set is not kept")

// topicsDir is the directory of the subscription sets of every input.
func topicsDir(dataDir string) string {
	return filepath.Join(dataDir, filepath.FromSlash(topicsSubdir))
}

// topicsFile is the name of the subscription set of input id.
func topicsFile(id string) string {
	return id + topicsExtension
}

// readTopics returns the subscription set the input last wrote. A missing
// or unreadable file is an error the caller logs: there is then nothing to
// unsubscribe.
func readTopics(dataDir, id string) ([]string, error) {
	if dataDir == "" {
		return nil, errNoDataDir
	}

	data, err := os.ReadFile(filepath.Join(topicsDir(dataDir), topicsFile(id)))
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

	dir := topicsDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(topics)
	if err != nil {
		return err
	}

	return atomicfile.Write(dir, topicsFile(id), data)
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
