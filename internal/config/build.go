// This file holds the wire types and the builder core: the walk that turns a validated document tree into a Document.

package config

import (
	"encoding/json"
	"errors"
)

type wireDocument struct {
	Schema  string `json:"schema"`
	Version int64  `json:"version"`
}

type wireInput struct {
	ID             string          `json:"id"`
	Driver         string          `json:"driver"`
	Connection     json.RawMessage `json:"connection"`
	DeathTolerance string          `json:"death_tolerance"`
	Devices        []struct {
		Device string `json:"device"`
		Points []struct {
			Signal     string                       `json:"signal"`
			Address    string                       `json:"address"`
			Read       json.RawMessage              `json:"read"`
			Priority   string                       `json:"priority"`
			Type       string                       `json:"type"`
			ByteOrder  string                       `json:"byte_order"`
			Unit       string                       `json:"unit"`
			Transforms []map[string]json.RawMessage `json:"transforms"`
		} `json:"points"`
		Derived []struct {
			Signal   string `json:"signal"`
			Type     string `json:"type"`
			Priority string `json:"priority"`
			Expr     string `json:"expr"`
			On       struct {
				Interval string `json:"interval"`
			} `json:"on"`
			Transforms []map[string]json.RawMessage `json:"transforms"`
		} `json:"derived"`
	} `json:"devices"`
}

type wireProcessor struct {
	ID             string          `json:"id"`
	Plugin         string          `json:"plugin"`
	Observe        []string        `json:"observe"`
	DeathTolerance string          `json:"death_tolerance"`
	Config         json.RawMessage `json:"config"`
	Devices        []struct {
		Device  string `json:"device"`
		Signals []struct {
			Signal   string `json:"signal"`
			Type     string `json:"type"`
			Priority string `json:"priority"`
			Unit     string `json:"unit"`
		} `json:"signals"`
	} `json:"devices"`
}

type wireOutput struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	URL    string          `json:"url"`
	Token  string          `json:"token"`
	Config json.RawMessage `json:"config"`
	Route  struct {
		Include []string `json:"include"`
	} `json:"route"`
	Batch struct {
		MaxSamples int    `json:"max_samples"`
		MaxBody    string `json:"max_body"`
		MaxWait    string `json:"max_wait"`
	} `json:"batch"`
	Buffer struct {
		MaxSize        string  `json:"max_size"`
		MinFreePercent float64 `json:"min_free_percent"`
		FsyncInterval  string  `json:"fsync_interval"`
	} `json:"buffer"`
}

type wirePlugin struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	ManifestSHA256 string `json:"manifest_sha256"`
	MaxMemory      string `json:"max_memory"`
}

// builder turns a defaulted tree into a Document and collects the semantic
// problems the schema cannot express. A lenient builder works on a tree the
// schema refused: values of the wrong type are left zero.
type builder struct {
	msgs    []string
	lenient bool
}

func (b *builder) add(p path, text string) { b.msgs = append(b.msgs, p.msg(text)) }

// component renders one component and decodes it into dst. The error is a
// fixed text: the json error would quote a value of the document.
func (b *builder) component(v any, dst any) (json.RawMessage, error) {
	raw, err := canonical(v)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, dst); err != nil && !b.lenient {
		return nil, errDecodeComponent
	}
	return raw, nil
}

// errDecodeComponent reports a validated component that does not fit its Go
// type: a mismatch between the schema and the code, never a user error.
var errDecodeComponent = errors.New("config: internal error: a validated component does not fit its Go type")

func build(root map[string]any, lenient bool) (Document, []string, error) {
	b := &builder{lenient: lenient}
	var top wireDocument
	raw, err := b.component(root, &top)
	if err != nil {
		return Document{}, nil, err
	}
	doc := Document{Schema: top.Schema, Version: top.Version, Raw: raw}

	for i, v := range list(root["inputs"]) {
		in, err := b.input(path{"inputs"}.index(i), v)
		if err != nil {
			return Document{}, nil, err
		}
		doc.Inputs = append(doc.Inputs, in)
	}
	for i, v := range list(root["outputs"]) {
		out, err := b.output(path{"outputs"}.index(i), v)
		if err != nil {
			return Document{}, nil, err
		}
		doc.Outputs = append(doc.Outputs, out)
	}
	for i, v := range list(root["plugins"]) {
		var w wirePlugin
		raw, err := b.component(v, &w)
		if err != nil {
			return Document{}, nil, err
		}
		var maxMemory int64
		if w.MaxMemory != "" {
			maxMemory = b.size(path{"plugins"}.index(i).key("max_memory"), w.MaxMemory)
		}
		doc.Plugins = append(doc.Plugins, Plugin{ID: w.ID, Path: w.Path, SHA256: w.SHA256, ManifestSHA256: w.ManifestSHA256, MaxMemory: maxMemory, Raw: raw})
	}

	for i, v := range list(root["processors"]) {
		pr, err := b.processor(path{"processors"}.index(i), v)
		if err != nil {
			return Document{}, nil, err
		}
		doc.Processors = append(doc.Processors, pr)
	}

	b.uniqueIDs(doc)
	b.processorRules(doc)
	b.uniqueSignals(doc)
	return doc, b.msgs, nil
}
