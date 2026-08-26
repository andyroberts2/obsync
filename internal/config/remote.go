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

// ParseRemote normalises a git remote URL to the pair gate 5 compares. It is
// the same function for both sides of that comparison: what obsync was
// configured with, and what the vault's own origin says.
func ParseRemote(raw string) (ConfiguredRemote, error) {
	_, remote, err := parseRemote(raw)
	return remote, err
}

// acceptedForms is what an operator is told when a URL is refused, and it is
// the whole accepted list: https, http, ssh, scp-style and file (§8). file://
// is accepted and documented because obsync's own tests reach a local bare
// remote over it, and a form obsync tests is a form obsync supports.
const acceptedForms = `obsync accepts https://, http://, ssh://, file:// and scp-style git@host:owner/repo`

// parseRemote returns the URL's scheme alongside the normalised remote. The
// scheme is dropped from the comparison and still decides two things: whether
// a credential file is required, and whether the http:// warning fires.
func parseRemote(raw string) (scheme string, remote ConfiguredRemote, err error) {
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

	return parsed.Scheme, ConfiguredRemote{Host: host, Path: path}, nil
}

// echoURL is the repo URL as the startup line prints it.
//
// obsync never puts a credential in a URL — the token is a file precisely so
// that it reaches neither a URL, a log line, an argv nor `git remote -v` (§8)
// — but an operator can, and this line is the one place obsync would otherwise
// read one straight back out. So for the two schemes §8 says authenticate with
// a credential, any embedded userinfo gets the same treatment as the token
// itself: absent, rather than redacted or shortened. ssh's userinfo is a login
// name and stays.
func echoURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil || !needsCredential(parsed.Scheme) {
		return raw
	}
	parsed.User = nil
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
