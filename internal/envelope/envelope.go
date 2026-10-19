// Package envelope builds the laraplate-machine/1 JSON envelope from the
// agent's internal records. It decides nothing about batching or sequence
// numbers: the caller supplies the message id and source_seq and gets the
// bytes to send, which are stored and resent unchanged.
package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/swolley/laraplate-edge/internal/model"
)

const (
	// Protocol is the value of the envelope's protocol field.
	Protocol = "laraplate-machine/1"

	// maxMessageIDLen is the protocol limit on message_id.
	maxMessageIDLen = 128

	// timeLayout is UTC with millisecond precision.
	timeLayout = "2006-01-02T15:04:05.000Z"
)

type wireEnvelope struct {
	Protocol  string       `json:"protocol"`
	MessageID string       `json:"message_id"`
	SourceSeq uint64       `json:"source_seq"`
	SentAt    string       `json:"sent_at"`
	Devices   []wireDevice `json:"devices"`
	Losses    []wireLoss   `json:"losses,omitempty"`
}

type wireDevice struct {
	Device  string       `json:"device"`
	Type    string       `json:"type"`
	Samples []wireSample `json:"samples,omitempty"`
	Signals []wireSignal `json:"signals,omitempty"`
}

type wireSample struct {
	Signal  string `json:"signal"`
	TS      string `json:"ts"`
	Value   any    `json:"value"`
	Quality string `json:"quality"`
}

type wireSignal struct {
	Signal   string `json:"signal"`
	DataType string `json:"data_type"`
	Unit     string `json:"unit,omitempty"`
}

type wireLoss struct {
	Priority string `json:"priority"`
	FromTS   string `json:"from_ts"`
	ToTS     string `json:"to_ts"`
	Samples  int    `json:"samples"`
	Reason   string `json:"reason"`
}

// Build returns the JSON body of one envelope.
//
// Devices are grouped from the records in order. A sample joins the open data
// entry of its device, which is created at the device's first appearance. A
// birth or death is an entry of its own at its position and closes the
// device's open data entry, so later samples of that device open a new data
// entry after the event and the per-device order is preserved.
//
// Losses travel with the envelope, which must carry at least one record.
// Build refuses invalid input rather than producing a body the backend would
// reject.
func Build(messageID string, seq uint64, sentAt time.Time, records []model.Record, losses []model.Loss) ([]byte, error) {
	if messageID == "" || len(messageID) > maxMessageIDLen {
		return nil, fmt.Errorf("message id must be 1 to %d bytes, got %d", maxMessageIDLen, len(messageID))
	}

	if len(records) == 0 {
		return nil, errors.New("envelope needs at least one record")
	}

	env := wireEnvelope{
		Protocol:  Protocol,
		MessageID: messageID,
		SourceSeq: seq,
		SentAt:    formatTime(sentAt),
	}

	devices, err := groupDevices(records)
	if err != nil {
		return nil, err
	}

	env.Devices = devices

	for i, l := range losses {
		if err := l.Validate(); err != nil {
			return nil, fmt.Errorf("loss %d: %w", i, err)
		}

		env.Losses = append(env.Losses, wireLoss{
			Priority: l.Priority.String(),
			FromTS:   formatTime(l.FromTS),
			ToTS:     formatTime(l.ToTS),
			Samples:  l.Samples,
			Reason:   l.Reason,
		})
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(env); err != nil {
		return nil, fmt.Errorf("encode envelope: %w", err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func groupDevices(records []model.Record) ([]wireDevice, error) {
	var (
		devices []wireDevice
		open    = make(map[string]int) // device -> index of its open data entry
	)

	for i, r := range records {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}

		if s := r.Sample; s != nil {
			idx, ok := open[s.Device]
			if !ok {
				devices = append(devices, wireDevice{Device: s.Device, Type: "data"})
				idx = len(devices) - 1
				open[s.Device] = idx
			}

			devices[idx].Samples = append(devices[idx].Samples, wireSample{
				Signal:  s.Signal,
				TS:      formatTime(s.TS),
				Value:   wireValue(s.Value),
				Quality: s.Quality.String(),
			})

			continue
		}

		e := r.Event
		delete(open, e.Device)

		entry := wireDevice{Device: e.Device, Type: e.Kind.String()}
		for _, si := range e.Signals {
			entry.Signals = append(entry.Signals, wireSignal{Signal: si.Signal, DataType: si.DataType, Unit: si.Unit})
		}

		devices = append(devices, entry)
	}

	return devices, nil
}

// wireValue returns the Go value that encodes as the JSON value. The value
// has been validated, so the kind is known.
func wireValue(v model.Value) any {
	switch v.Kind {
	case model.KindFloat:
		return v.Float
	case model.KindInt:
		return v.Int
	case model.KindBool:
		return v.Bool
	default:
		return v.Str
	}
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
