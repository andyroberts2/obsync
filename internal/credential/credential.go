// Package credential is obsync's credential path: obsync is its own git
// credential helper.
//
// git is configured with `credential.helper = !obsync credential-helper`, and
// asks obsync for the credential exactly when it needs one. There is no daemon,
// no socket, no orphan process, and no writable socket path an arbitrary UID
// may not have — and no cache, which is the point rather than an omission: a
// cache exists specifically to *not* re-read, and re-reading is what makes a
// rotated credential heal with no restart (§8).
//
// So the secret's whole journey is: the file the operator mounted, this
// process's memory for the length of one invocation, and git's stdin. It never
// reaches a URL, an argv, a socket, or a file obsync wrote — which is what
// makes DEBUG logging every git's full argv safe (§9).
package credential

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/andyroberts2/obsync/internal/config"
)

// maxCredentialBytes bounds both what is read from the credential file and
// what is read from git's request. A credential is tens of bytes and a request
// is a handful of short lines; the bound is four orders of magnitude above
// either, and exists so that a path that turns out to name something enormous
// is a refusal rather than an allocation.
const maxCredentialBytes = 64 << 10

// Helper answers one credential-helper invocation and returns obsync's exit
// status. operation is the word git appends to the configured command — `get`,
// `store` or `erase`.
//
// It writes to stdout and to nothing else, ever. git relays a helper's stderr
// to its own, and obsync logs what git said, so a helper that writes to stderr
// is a helper whose output can reach a log — and the credential helper's own
// output is never logged, at any level (§9). A failure here is an exit status,
// which git ignores and obsync meets later as the remote refusing the
// credential: a bad credential is a self-healing network-half failure, not a
// freeze (§7).
//
// It logs nothing for the same reason, and resolves no configuration beyond the
// two variables the credential is: an invocation is a subprocess of a git that
// is already running, not a second obsync starting up.
func Helper(operation string, environ []string, stdin io.Reader, stdout io.Writer) int {
	// git writes the request whatever the operation is, and a helper that
	// leaves it unread is one git meets a write error on. Nothing is parsed
	// out of it: obsync drives exactly one remote, so a git obsync started can
	// only be asking about that one.
	_, _ = io.Copy(io.Discard, io.LimitReader(stdin, maxCredentialBytes))

	// git defines three operations and tells a helper to ignore any other, so
	// that a future git can add one. `store` is git offering the credential
	// back for safekeeping and `erase` is git dropping it: obsync keeps
	// nothing between invocations, so both are already done. Never a
	// `.git-credentials` file, never a `~/.netrc` (§8).
	if operation != "get" {
		return 0
	}

	path, username := config.CredentialFrom(environ)
	if path == "" {
		// ssh:// and file:// take no credential file, and git may still ask.
		// A helper with nothing to say says nothing and succeeds; git falls
		// back to the forced askpass, which answers empty rather than waiting
		// on a terminal that is not there.
		return 0
	}

	credential, err := read(path)
	if err != nil {
		// The reason goes nowhere, and that is the rule rather than an
		// omission: what obsync has to say about a credential it could not
		// read, it says when the remote refuses the push it was for (§7, §9).
		return 1
	}
	if err := usable(username); err != nil {
		return 1
	}

	// The protocol is one key per line. Both values were checked for the one
	// byte that could turn a value into a second key.
	if _, err := fmt.Fprintf(stdout, "username=%s\npassword=%s\n", username, credential); err != nil {
		return 1
	}
	return 0
}

// read is the credential file, read now rather than at startup or once per
// run: per invocation is what "re-read every time git asks" means, and it is a
// property of the shape rather than a rule anything has to remember (§8).
func read(path string) (string, error) {
	// The shape is checked before the open, and that order is load-bearing
	// rather than tidy — the same rule startup applies to this file. Opening a
	// FIFO blocks until a writer appears, and a helper that blocks holds the
	// git that is waiting on it, which is a network git obsync would then be
	// killing at its deadline for no reason (§1).
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a file, so no credential can be read from it", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	// One byte past the bound, so a file over it is a refusal rather than a
	// silent truncation — a truncated credential is a credential that fails,
	// which is a worse answer than none.
	content, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxCredentialBytes {
		return "", fmt.Errorf("%q holds more than %d bytes, which is not a credential", path, maxCredentialBytes)
	}

	// Trimmed, because a token file written by an editor ends with a newline
	// and one written by `printf` does not, and both name the same token.
	credential := strings.TrimSpace(string(content))
	if credential == "" {
		return "", fmt.Errorf("%q holds no credential", path)
	}
	if err := usable(credential); err != nil {
		return "", err
	}
	return credential, nil
}

// usable rejects a value git's protocol cannot carry. The protocol is
// line-based, so a value holding a newline arrives at git as a second key —
// which is a value that means something other than what the operator put in
// the file, and the one way this could hand git an instruction rather than a
// credential.
func usable(value string) error {
	if strings.ContainsAny(value, "\n\r\x00") {
		return errors.New("a credential is one line, and this one is not")
	}
	return nil
}
