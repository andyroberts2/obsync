/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file is obsync's transcription quarantine (§12). The credential
// isolation below is transcribed from kubernetes/git-sync's Go source rather
// than reimplemented, because being wrong about it is more expensive than the
// duplication — and it lives in one file, under the upstream copyright header
// and with the exact origin recorded, so the licence obligation stays auditable
// as well as satisfied. Do not scatter it, and do not fold it into the
// surrounding package.
//
// Upstream: github.com/kubernetes/git-sync, Apache-2.0, at commit
// cf98d8389384662e1b0d20389a6cf88246d303fe (the checkout the prior-art report
// in docs/research/kubernetes-git-sync.md was written from). git-sync ships no
// NOTICE file — measured — so §4(d) never bites; every source file carries the
// header above, so §4(a) and §4(b) do.
//
// The transcribed lines, and what each one is:
//
//	main.go:785-795   the private per-process GIT_CONFIG_GLOBAL, plus
//	                  GIT_CONFIG_NOSYSTEM:
//
//	  // Don't pollute the user's .gitconfig if this is being run directly.
//	  if f, err := os.CreateTemp("", "git-sync.gitconfig.*"); err != nil {
//	          log.Error(err, "FATAL: can't create gitconfig file")
//	          os.Exit(1)
//	  } else {
//	          gitConfig := f.Name()
//	          f.Close()
//	          os.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
//	          os.Setenv("GIT_CONFIG_NOSYSTEM", "true")
//	          log.V(2).Info("created private gitconfig file", "path", gitConfig)
//	  }
//
//	main.go:2286-2294  the two credential settings, written into that private
//	                   config by running git itself (SetupDefaultGitConfigs,
//	                   main.go:2278-2302):
//
//	  }, {
//	          // How to manage credentials (for those modes that need it).
//	          key: "credential.helper",
//	          val: "cache --timeout 3600",
//	  }, {
//	          // Never prompt for a password.
//	          key: "core.askPass",
//	          val: "true",
//	  }}
//
//	main.go:2055-2067  the secret handed to git through git's own credential
//	                   mechanism rather than through a URL (StoreCredentials).
//
//	main.go:983-991    the credential file re-read from disk rather than held,
//	                   which is what makes a rotated token work:
//
//	  // If this credential has a password file, re-read it from disk
//	  // to pick up token rotation
//	  if cred.PasswordFile != "" {
//	          passwordFileBytes, err := os.ReadFile(cred.PasswordFile)
//
// Two divergences, both deliberate, both stated here rather than left for a
// reader to notice:
//
//   - `credential.helper` is obsync itself, not `cache --timeout 3600`. A cache
//     daemon exists specifically to *not* re-read, which is the opposite of
//     what §8 requires; a helper that *is* obsync re-reads by construction, and
//     brings no daemon, no socket and no orphan process with it. The re-read
//     upstream does per sync (main.go:983-991) obsync therefore does per
//     credential request, which is strictly more often.
//   - obsync writes its private config with `git config --file <path>` and
//     hands git `GIT_CONFIG_GLOBAL` per invocation, where upstream sets the
//     variable in its own process environment and writes with `--global`. Same
//     file, same isolation; obsync's loop never mutates its own environment
//     because everything it runs takes an environment built for it.
//
// The rest of obsync's private git configuration — the commit identity,
// fetch.fsckObjects and gc.autoDetach — is obsync's own and lives in git.go.
package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isolation is the credential isolation every git obsync runs is placed
// inside: a private git configuration that no other git and no ambient
// ~/.gitconfig can reach, holding the helper that is obsync itself and the
// forced askpass that keeps an interactive prompt from ever hanging the loop.
type isolation struct {
	// dir holds the private configuration and is removed when the Repo closes.
	// It sits outside the vault: bootstrap has to configure git before there
	// is a .git to write into (#26), and anything obsync wrote inside the
	// vault would be an owned path it would then have to declare (§10).
	dir        string
	configPath string

	// helper is the credential.helper value: a `!` command, which git runs
	// through a shell with the operation appended.
	helper string
}

// newIsolation creates the private configuration directory and works out what
// git should run to reach obsync's credential helper.
func newIsolation() (*isolation, error) {
	// obsync's own path rather than the name `obsync`, so the helper git
	// starts is this build and not whatever a PATH in the container happens to
	// resolve. It is quoted because a path is not a shell word: git runs a `!`
	// value through a shell, and an unquoted space would make the helper a
	// command that does not exist.
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("obsync cannot find its own path, which is what git runs to reach "+
			"its credential helper: %w", err)
	}

	dir, err := os.MkdirTemp("", "obsync-git-config")
	if err != nil {
		return nil, fmt.Errorf("creating obsync's private git configuration: %w", err)
	}

	return &isolation{
		dir:        dir,
		configPath: filepath.Join(dir, "config"),
		helper:     "!" + shellQuote(self) + " credential-helper",
	}, nil
}

// settings are the credential half of obsync's private git configuration.
func (i *isolation) settings() [][2]string {
	return [][2]string{
		// obsync is its own credential helper, so the credential is read from
		// the file the operator mounted exactly when git asks for it (§8).
		{"credential.helper", i.helper},
		// Forced, so an interactive prompt can never hang the loop. git runs
		// it through a shell, and `true` produces an empty credential, which
		// fails fast rather than waiting on a terminal that is not there.
		{"core.askPass", "true"},
	}
}

// environment is what every git obsync runs is given so that the private
// configuration above is the only configuration in play besides the vault's
// own — which outranks it deliberately, and is the human's file (§1).
func (i *isolation) environment() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=" + i.configPath,
		"GIT_CONFIG_NOSYSTEM=1",
	}
}

func (i *isolation) close() error {
	return os.RemoveAll(i.dir)
}

// shellQuote wraps a path so that a shell reads it as one word whatever is in
// it. Single quotes protect everything except a single quote, which is closed,
// escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
