// Package config's resolve.go previously constructed a shared
// auth.Config + auth.Auth for all plugins to share. With per-plugin
// configuration, that responsibility moved into each plugin's Configure
// (see authbridge/authlib/plugins/CONVENTIONS.md), and this file is now
// just the shared credential-file waiters that multiple plugins need
// when they share a file path (e.g. /shared/client-id.txt used by both
// jwt-validation's audience_file and token-exchange's client_id_file).
package config

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// ReadCredentialFile performs a one-shot read of a credential file,
// returning its whitespace-trimmed contents. Used by plugins from
// Configure to opportunistically pick up values that client-registration
// has already written; when it returns an error, the plugin should fall
// back to WaitForCredentialFile from Init to wait for the file.
func ReadCredentialFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("file %s is empty", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// WaitForCredentialFile blocks until the file is readable with non-zero
// length, or until ctx is cancelled. Plugins call this from Init (via a
// goroutine) to wait out the race with client-registration's secret
// provisioning.
//
// Polls at 2s intervals — fast enough for human-observable boot times,
// slow enough that a pod full of plugins isn't hammering the kubelet.
func WaitForCredentialFile(ctx context.Context, path string) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if v, err := ReadCredentialFile(path); err == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}
