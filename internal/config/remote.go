package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
)

// ConfiguredRemote is a git remote URL reduced to what identifies where bytes
// go: a host and a path, and nothing else.
//
// It is comparable, and comparing two of them is gate 5 (§8). What the
// normalisation discards is what varies without changing the destination —
// scheme, embedded credentials, a default port, host case, a .git suffix and a
// trailing slash. Discarding the scheme is the deliberate one: https and ssh
// against the same host and path are the same repo, and freezing because an
// operator swapped a PAT for a deploy key protects nothing.
type ConfiguredRemote struct {
	Host string
	Path string
}

func (r ConfiguredRemote) String() string { return r.Host + "/" + r.Path }

// LogValue keeps a remote one field in logfmt rather than two.
func (r ConfiguredRemote) LogValue() slog.Value { return slog.StringValue(r.String()) }

// CredentialPath is how a remote authenticates, which the URL scheme alone
// decides (§8, docs/credentials.md): with the credential file obsync's helper
// serves, with key material obsync knows nothing about, or with nothing at all.
//
// It is the half of a scheme that ConfiguredRemote is right to discard and
// obsync is not. Where bytes go is a host and a path; how obsync gets them
// there is this, and the two are independent — an operator may swap a PAT for a
// deploy key against the same repository, and the same swap leaves the token
// obsync was given, and required at startup, read by nothing.
type CredentialPath int

const (
	// NoCredential is file://, which authenticates with nothing at all.
	NoCredential CredentialPath = iota
	// ACredentialFile is http:// and https://: obsync is git's credential
	// helper and serves OBSYNC_TOKEN_FILE when git asks.
	ACredentialFile
	// KeyMaterial is ssh:// and its scp-style spelling: ssh reads a key out of
	// the home directory of the UID obsync runs as, and obsync neither
	// supplies it nor knows whether it is there.
	KeyMaterial
)

// CredentialPathOf answers the credential path a scheme takes. The scheme is
// the whole input, which is what makes this decidable without touching a
// network or a disk.
func CredentialPathOf(scheme string) CredentialPath {
	switch scheme {
	case "http", "https":
		return ACredentialFile
	case "ssh":
		return KeyMaterial
	}
	return NoCredential
}

// acceptedForms is what an operator is told when a URL is refused, and it is
// the whole accepted list: https, http, ssh, scp-style and file (§8). file://
// is accepted and documented because obsync's own tests reach a local bare
// remote over it, and a form obsync tests is a form obsync supports.
const acceptedForms = `obsync accepts https://, http://, ssh://, file:// and scp-style git@host:owner/repo`

// ParseRemote normalises a git remote URL to the pair gate 5 compares, and
// answers with the scheme beside it. It is the same function for both sides of
// that comparison: what obsync was configured with, and what the vault's own
// origin says.
//
// The scheme travels separately rather than inside ConfiguredRemote, and that
// is load-bearing: the pair is compared with `==`, so a scheme inside it would
// make the transport part of where bytes go and turn a swapped credential into
// gate 5's freeze. What the scheme decides is the credential path above, which
// is a different question asked in a different place.
func ParseRemote(raw string) (scheme string, remote ConfiguredRemote, err error) {
	// scp-style git@host:owner/repo is rewritten rather than parsed
	// separately, which is how it comes out equal to ssh://git@host/owner/repo
	// rather than merely being asserted to be.
	if rewritten, ok := scpStyle(raw); ok {
		raw = rewritten
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ConfiguredRemote{}, fmt.Errorf("%q is not a URL: %w. %s", raw, err, acceptedForms)
	}
	switch parsed.Scheme {
	case "https", "http", "ssh", "file":
	case "":
		return "", ConfiguredRemote{}, fmt.Errorf("%q names no scheme. %s", raw, acceptedForms)
	default:
		return "", ConfiguredRemote{}, fmt.Errorf("%q uses the %s:// scheme, which obsync does not "+
			"accept. %s", raw, parsed.Scheme, acceptedForms)
	}

	// Embedded credentials are discarded by construction: url.URL keeps
	// userinfo out of Host, and nothing here puts it back.
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" && port != defaultPort(parsed.Scheme) {
		host = net.JoinHostPort(host, port)
	}

	path := strings.Trim(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")

	// A URL missing either half names no repository, and which half is missing
	// is decidable from the environment block alone — so it exits rather than
	// parking (§8). The alternative is worse than it looks: gate 5 would spend
	// every run comparing the vault's origin against half a pair, and the
	// operator would meet a full freeze where they should have met a message
	// naming the variable. file:// is the one scheme whose host is meant to be
	// empty.
	if host == "" && parsed.Scheme != "file" {
		return "", ConfiguredRemote{}, fmt.Errorf("%q names no host. %s", raw, acceptedForms)
	}
	if path == "" {
		return "", ConfiguredRemote{}, fmt.Errorf("%q names no repository path. %s", raw, acceptedForms)
	}

	return parsed.Scheme, ConfiguredRemote{Host: host, Path: path}, nil
}

// echoURL is the repo URL as the startup line prints it.
//
// obsync never puts a credential in a URL — the token is a file precisely so
// that it reaches neither a URL, a log line, an argv nor `git remote -v` (§8)
// — but an operator can, and this line is the one place obsync would otherwise
// read one straight back out. So for the two schemes §8 says authenticate with
// a credential, any embedded userinfo gets the same treatment as the token
// itself: absent, rather than redacted or shortened.
//
// ssh's userinfo is a login name and stays — but only the name. A password
// there is never a login name and git's ssh transport cannot use one anyway,
// so it is a secret in a URL under a scheme that has no use for it, and it
// gets the token's treatment rather than the name's.
//
// A URL with nothing to drop is returned exactly as it was set, rather than
// re-encoded by url.String(): the line exists to be diffed against a compose
// file, and a value that came back spelled differently would read as obsync
// having understood something else.
func echoURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	if needsCredential(parsed.Scheme) {
		parsed.User = nil
		return parsed.String()
	}

	if _, hasPassword := parsed.User.Password(); !hasPassword {
		return raw
	}
	if name := parsed.User.Username(); name != "" {
		parsed.User = url.User(name)
	} else {
		parsed.User = nil
	}
	return parsed.String()
}

// defaultPort is the port a scheme reaches without being told, and therefore
// the one that carries no information about where the repo is.
func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	case "ssh":
		return "22"
	}
	return ""
}

// scpStyle rewrites git's scp-style syntax as the ssh:// URL it means, and
// reports whether the string was in that syntax at all.
//
// The rule is git's: no scheme, and a colon before any slash. A string with
// neither — a bare local path — is deliberately not rewritten. It is refused
// with the accepted forms, because file:// is the local form obsync accepts.
func scpStyle(raw string) (string, bool) {
	if strings.Contains(raw, "://") {
		return "", false
	}
	colon := strings.Index(raw, ":")
	slash := strings.Index(raw, "/")
	if colon < 0 || (slash >= 0 && slash < colon) {
		return "", false
	}
	return "ssh://" + raw[:colon] + "/" + strings.TrimPrefix(raw[colon+1:], "/"), true
}
