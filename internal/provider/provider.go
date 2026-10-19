// Package provider brings configuration documents to the supervisor: from a
// local file (File) or from the Laraplate backend (Laraplate), after the last
// valid document has been applied at start (spec 5.4).
package provider

import (
	"context"
	"log/slog"

	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/supervisor"
)

// Applier is what a provider uses of the supervisor.
type Applier interface {
	Apply(ctx context.Context, doc config.Document) (supervisor.Outcome, error)
	AppliedVersion() int64
}

// ApplyLastValid applies the last valid document of store, when there is
// one, so the agent runs offline from its last configuration before the
// provider takes over. Problems are logged.
func ApplyLastValid(ctx context.Context, store *config.Store, a Applier, log *slog.Logger) {
	doc, ok, err := store.LastValid()
	if err != nil {
		log.Error("provider: cannot read the last valid configuration", "error", err)

		return
	}
	if !ok {
		log.Info("provider: no last valid configuration yet")

		return
	}

	out, err := a.Apply(ctx, doc)
	logOutcome(log, "last valid", doc.Version, out, err)
}

// logOutcome logs the outcome of an Apply; the messages name paths, never
// values.
func logOutcome(log *slog.Logger, source string, version int64, out supervisor.Outcome, err error) {
	if err != nil {
		log.Error("provider: configuration applied with an error", "source", source, "version", version, "error", err)
	}

	switch out.Result {
	case supervisor.ResultApplied:
		log.Info("provider: configuration applied", "source", source, "version", version)
	case supervisor.ResultRejected:
		log.Warn("provider: configuration rejected", "source", source, "version", version, "errors", out.Errors)
	case supervisor.ResultRolledBack:
		log.Warn("provider: configuration rolled back", "source", source, "version", version, "errors", out.Errors)
	}
}
