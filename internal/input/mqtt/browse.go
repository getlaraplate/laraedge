package mqtt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Limits and defaults of browse (spec section 6).
const (
	// BrowseMaxTopics is how many topics browse lists; the next new topic
	// stops it.
	BrowseMaxTopics = 1000
	// DefaultBrowseFilter is the topic filter browse subscribes to.
	DefaultBrowseFilter = "#"
	// DefaultBrowseDuration is how long browse collects messages.
	DefaultBrowseDuration = 10 * time.Second
	// MinBrowseDuration and MaxBrowseDuration bound --duration.
	MinBrowseDuration = time.Second
	MaxBrowseDuration = 5 * time.Minute
	// PreviewMax is the longest preview of a payload or a value, in
	// characters.
	PreviewMax = 40
	// browseMaxPaths bounds the JSON leaves listed for one topic.
	browseMaxPaths = 100
)

// PathPreview is one JSON leaf of a payload: Address is `<topic>|<path>`, a
// usable point address, and Preview the escaped value, a string quoted.
type PathPreview struct {
	Address string
	Preview string
}

// BrowseEntry is one topic seen by browse. Messages counts what arrived;
// Retained and Size are those of the last message. For a JSON object or
// list, Paths lists its leaves in payload order (at most 100); otherwise, and
// for a payload above max_payload nothing, Text is the escaped preview of the
// payload. Topic and the previews hold the raw text: escape the topic with
// Escape before printing it.
type BrowseEntry struct {
	Topic    string
	Messages int
	Retained bool
	Size     int
	Paths    []PathPreview
	Text     string
}

// CheckFilter reports why filter is not a valid MQTT topic filter, as a fixed
// text, or nil.
func CheckFilter(filter string) error {
	if filter == "" {
		return errors.New("must not be empty")
	}
	if strings.ContainsRune(filter, 0) {
		return errors.New("must not contain a NUL character")
	}

	levels := strings.Split(filter, "/")
	for i, level := range levels {
		switch {
		case level == "#" && i != len(levels)-1:
			return errors.New("# must be the last level")
		case strings.Contains(level, "#") && level != "#":
			return errors.New("# must be a whole level")
		case strings.Contains(level, "+") && level != "+":
			return errors.New("+ must be a whole level")
		}
	}

	return nil
}

// Browse subscribes to filter with the client id <client_id>-browse (clean
// session, QoS 0) and collects the messages for duration on the input's
// clock, or until ctx ends, whichever is first; retained messages arrive at
// once. It returns the topics sorted, and whether it stopped early because a
// 1001st topic arrived. It never publishes and ignores the input's own
// subscriptions. A connection failure, and a connection lost before the end,
// are errors; a ctx that ends is not.
func (in *Input) Browse(ctx context.Context, filter string, duration time.Duration) (entries []BrowseEntry, truncated bool, err error) {
	if err := CheckFilter(filter); err != nil {
		return nil, false, fmt.Errorf("mqtt input %q: browse: filter: %w", in.cfg.ID, err)
	}
	if duration < MinBrowseDuration || duration > MaxBrowseDuration {
		return nil, false, fmt.Errorf("mqtt input %q: browse: duration: want 1s to 5m", in.cfg.ID)
	}

	var (
		mu     sync.Mutex
		topics = make(map[string]*BrowseEntry)
		full   = make(chan struct{})
	)

	onMessage := func(topic string, payload []byte, retained bool) {
		mu.Lock()
		defer mu.Unlock()

		if truncated {
			return
		}

		e := topics[topic]
		if e == nil {
			if len(topics) >= BrowseMaxTopics {
				truncated = true
				close(full)

				return
			}
			e = &BrowseEntry{Topic: topic}
			topics[topic] = e
		}

		e.Messages++
		e.Retained = retained
		e.Size = len(payload)
		e.Paths, e.Text = nil, ""
		if len(payload) <= in.conn.MaxPayload {
			e.Paths, e.Text = describePayload(topic, payload)
		}

		in.traceEvent(traceBrowseMessage)
	}

	c, err := in.commandClient(ctx, "-browse", []string{filter}, onMessage)
	if err != nil {
		return nil, false, fmt.Errorf("mqtt input %q: browse: %w", in.cfg.ID, err)
	}
	defer c.Disconnect()

	timer := in.clk.NewTimer(duration)
	defer timer.Stop()

	var lostErr error
	select {
	case <-timer.C():
	case <-ctx.Done():
	case <-full:
	case <-c.Lost():
		lostErr = errConnectionLost
	}

	if lostErr != nil {
		return nil, false, fmt.Errorf("mqtt input %q: browse: %w", in.cfg.ID, lostErr)
	}

	mu.Lock()
	defer mu.Unlock()

	entries = make([]BrowseEntry, 0, len(topics))
	for _, e := range topics {
		entries = append(entries, *e)
	}
	slices.SortFunc(entries, func(a, b BrowseEntry) int { return strings.Compare(a.Topic, b.Topic) })

	return entries, truncated, nil
}

// describePayload returns the JSON leaves of a payload that is an object or a
// list, or else the preview of the payload text.
func describePayload(topic string, payload []byte) ([]PathPreview, string) {
	if v, ok := parseJSON(payload); ok {
		switch v.(type) {
		case map[string]any, []any:
			var paths []PathPreview
			collectLeaves(v, nil, topic, &paths)
			if len(paths) > 0 {
				return paths, ""
			}
		}
	}

	head := payload[:min(len(payload), 4*PreviewMax)]

	return nil, preview(string(head), false)
}

// collectLeaves appends the scalar leaves of v, depth first, objects in key
// order. A key that is empty or holds a dot has no dot path and is skipped; an
// empty object or list is a leaf.
func collectLeaves(v any, path []string, topic string, out *[]PathPreview) {
	if len(*out) >= browseMaxPaths {
		return
	}

	switch node := v.(type) {
	case map[string]any:
		if len(node) == 0 {
			addLeaf(path, topic, "{}", out)

			return
		}
		for _, key := range slices.Sorted(mapKeys(node)) {
			if key == "" || strings.Contains(key, ".") {
				continue
			}
			collectLeaves(node[key], append(path, key), topic, out)
		}
	case []any:
		if len(node) == 0 {
			addLeaf(path, topic, "[]", out)

			return
		}
		for i, item := range node {
			collectLeaves(item, append(path, strconv.Itoa(i)), topic, out)
		}
	case string:
		addLeaf(path, topic, preview(node, true), out)
	case nil:
		addLeaf(path, topic, "null", out)
	case bool:
		addLeaf(path, topic, strconv.FormatBool(node), out)
	default:
		addLeaf(path, topic, preview(fmt.Sprint(node), false), out)
	}
}

func mapKeys(m map[string]any) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func addLeaf(path []string, topic, text string, out *[]PathPreview) {
	if len(path) == 0 || len(*out) >= browseMaxPaths {
		return
	}

	*out = append(*out, PathPreview{Address: topic + "|" + strings.Join(path, "."), Preview: text})
}

// preview escapes s and cuts it to PreviewMax characters, ending with "..."
// when it was cut; quote wraps it in double quotes, which count. A control
// character, an invalid UTF-8 byte, a backslash or a quote is written as a Go
// escape (\n, \x00, \\, \"), so nothing from a machine reaches the terminal.
func preview(s string, quote bool) string {
	limit := PreviewMax
	if quote {
		limit -= 2
	}

	var (
		pieces []string
		total  int
	)
	for rest := s; rest != "" && total <= limit; {
		r, size := utf8.DecodeRuneInString(rest)
		piece := escapeRune(r, size, rest[:size])
		pieces = append(pieces, piece)
		total += utf8.RuneCountInString(piece)
		rest = rest[size:]
	}

	cut := total > limit
	if cut {
		var kept []string
		width := 0
		for _, piece := range pieces {
			w := utf8.RuneCountInString(piece)
			if width+w > limit-3 {
				break
			}
			kept = append(kept, piece)
			width += w
		}
		pieces = kept
	}

	out := strings.Join(pieces, "")
	if cut {
		out += "..."
	}
	if quote {
		out = `"` + out + `"`
	}

	return out
}

// Escape returns s with the characters Escape of a preview would escape
// written as Go escapes; it never shortens.
func Escape(s string) string {
	var b strings.Builder
	for rest := s; rest != ""; {
		r, size := utf8.DecodeRuneInString(rest)
		b.WriteString(escapeRune(r, size, rest[:size]))
		rest = rest[size:]
	}

	return b.String()
}

func escapeRune(r rune, size int, raw string) string {
	switch {
	case r == utf8.RuneError && size == 1:
		return fmt.Sprintf(`\x%02x`, raw[0])
	case r == '"' || r == '\\' || !strconv.IsPrint(r):
		q := strconv.Quote(raw)

		return q[1 : len(q)-1]
	default:
		return raw
	}
}
