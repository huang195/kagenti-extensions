package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Only a local install gets a store: it is the one place a restart is routine and
// the state can live in the user's own ~/.cortex.
func TestOpenPluginStore_OnlyOnALocalInstall(t *testing.T) {
	inside, outside := localConfig(t)
	if st := openPluginStore(outside); st != nil {
		_ = st.Close()
		t.Error("opened a store for a config outside ~/.cortex")
	}
	st := openPluginStore(inside)
	if st == nil {
		t.Fatal("no store for a local install")
	}
	if err := st.Set(context.Background(), "k", "v", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(inside), pluginStoreFileName)); err != nil {
		t.Errorf("the store is not at ~/.cortex/%s: %v", pluginStoreFileName, err)
	}
}

// A store that cannot open is a warning, not a failed start: plugins then keep their
// state in memory, as they did before there was a store.
func TestOpenPluginStore_AFailureToOpenIsNoStore(t *testing.T) {
	inside, _ := localConfig(t)
	// A directory where the file should be: nothing can be read or written there.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(inside), pluginStoreFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if st := openPluginStore(inside); st != nil {
		_ = st.Close()
		t.Error("opened a store over a directory")
	}
}
