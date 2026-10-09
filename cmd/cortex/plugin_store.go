package main

import (
	"log/slog"
	"path/filepath"

	"github.com/rossoctl/cortex/core/storage"
	"github.com/rossoctl/cortex/core/storage/filestore"
)

// pluginStoreFileName is the plugins' saved state under ~/.cortex, beside the cost
// ledger and the session archive.
const pluginStoreFileName = "plugin-state.json"

// closePluginStoreOnFatal saves the plugin store before a fatal exit, for the reason
// closeLedgerOnFatal flushes the ledger. Nil whenever no store is open.
var closePluginStoreOnFatal func()

// openPluginStore opens the store plugins keep state in across restarts (see
// storage.StoreConsumer): ~/.cortex/plugin-state.json, on a local install only, the
// one place a restart is routine and the state can live in the user's own directory.
// Elsewhere it returns nil, and so it does when the store cannot open — a warning,
// not a failed start: plugins then keep their state in memory, as they did before
// there was a store.
//
// The result is an interface that is nil when there is no store, never a nil
// *filestore.Store: plugins.Deps injects whatever is non-nil, and a typed nil would
// pass that check and panic in the plugin.
func openPluginStore(configPath string) storage.Store {
	if !startedFromLocalInstall(configPath) {
		return nil
	}
	dir, err := defaultCortexDir()
	if err != nil {
		slog.Warn("plugin store disabled — cannot determine where to write it; plugins keep their state in memory", "error", err)
		return nil
	}
	path := filepath.Join(dir, pluginStoreFileName)
	st, err := filestore.Open(path)
	if err != nil {
		slog.Warn("plugin store disabled — plugins keep their state in memory and lose it on restart", "path", path, "error", err)
		return nil
	}
	closePluginStoreOnFatal = func() {
		if cerr := st.Close(); cerr != nil {
			slog.Warn("plugin store: final save failed during a fatal startup error", "error", cerr)
		}
	}
	slog.Info("plugin store enabled — plugins' saved state survives a restart", "path", path)
	return st
}
