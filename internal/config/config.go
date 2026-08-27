// Package config resolves obsync's config surface: the nine OBSYNC_*
// environment variables of §8, one of them required.
//
// It draws §8's line and nothing else draws it: if a check needs a syscall on
// the vault or a network round trip it is a gate and obsync parks alive; if it
// can be decided from the environment block alone it is a config error and
// obsync exits 1. So nothing here looks at the vault, nothing here talks to a
// remote, and everything here is settled before the sync loop starts.
package config

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
)

// The nine variables. Their names, their defaults and their required-ness are
// part of the declared surface (§10) rather than an implementation detail: a
// tenth is a spec change, and a renamed one is a retirement (see retired).
const (
	repoVar        = "OBSYNC_REPO"
	vaultPathVar   = "OBSYNC_VAULT_PATH"
	branchVar      = "OBSYNC_BRANCH"
	tokenFileVar   = "OBSYNC_TOKEN_FILE"
	usernameVar    = "OBSYNC_USERNAME"
	sizeCeilingVar = "OBSYNC_SIZE_CEILING"
	authorNameVar  = "OBSYNC_AUTHOR_NAME"
	authorEmailVar = "OBSYNC_AUTHOR_EMAIL"
	logLevelVar    = "OBSYNC_LOG_LEVEL"
)

// prefix marks an environment variable as addressed to obsync. A name carrying
// it that obsync does not know is a typo worth a WARN; a name without it
// belongs to something else in the container and is none of obsync's business.
const prefix = "OBSYNC_"

// knobs is the config surface as a closed list, in the order the startup line
// echoes them.
var knobs = []string{
	repoVar, vaultPathVar, branchVar, tokenFileVar, usernameVar,
	sizeCeilingVar, authorNameVar, authorEmailVar, logLevelVar,
}

const (
	// defaultVaultPath is singular, deliberately not ignis's /vaults: obsync
	// manages one vault per container (§8).
	defaultVaultPath = "/vault"
	// defaultUsername works because GitHub takes any non-empty username with a
	// PAT. GitLab needs oauth2 and Gitea the real one, which is why it is a
	// knob at all (§8).
	defaultUsername = "obsync"
	// defaultSizeCeiling sits under GitHub's 100MB hard block and above its
	// 50MB soft warning (§5). It is prevention rather than a guarantee —
	// obsync can never discover a remote's real limit — which is exactly why
	// the number is a knob and may need lowering.
	defaultSizeCeiling = 95 << 20
	defaultAuthorName  = "obsync"
	// .invalid is reserved, so this is honest about not being an address and
	// can never collide with a real one. A container hostname was rejected: it
	// is a 12-hex id that churns on every recreate, and `git log --author`
	// filters on the name anyway (§8).
	defaultAuthorEmail = "obsync@obsync.invalid"
	defaultLogLevel    = slog.LevelInfo
)

// RemoteName is the name of the one remote obsync ever uses. It is not a knob
// and there is no plan for it to become one: obsync syncs one branch of one
// repo through one remote (§3), and a configurable name would buy a second way
// to write the same deployment.
const RemoteName = "origin"

// retired maps a variable name obsync no longer reads to the one that replaced
// it. A retired name is recognised rather than ignored, and checked before the
// unknown-name sweep: without that, the warn-on-unknown rule turns every
// rename into a silent revert-to-default across an upgrade (§8).
//
// It is empty because obsync has retired nothing: no name on the config
// surface has ever had a different one. That is also why nothing drives this
// branch at seam 1 — an empty closed list has no rows to run — and the first
// retirement is one row here plus the row that drives it.
var retired = map[string]string{}

// Config is the resolved config surface. It is built only by Resolve, and only
// out of an environment block that had no config error in it, so there is no
// half-resolved value here to mistake for a setting.
type Config struct {
	// RepoURL is OBSYNC_REPO exactly as it was set, which is what git is
	// handed for clone, fetch and push.
	RepoURL string
	// ConfiguredRemote is RepoURL reduced to what identifies where bytes go —
	// the pair gate 5 compares the vault's own origin against every run (§8).
	ConfiguredRemote ConfiguredRemote
	VaultPath        string
	// Branch is the operator's override and is empty when there is none, in
	// which case the tracked branch is resolved at startup — from the vault
	// when attaching to a repo, from the remote when cloning into an empty
	// directory (§3).
	Branch string
	// CredentialFile is the path OBSYNC_TOKEN_FILE named, and never the secret
	// in it: obsync reads that file when git asks for a credential and at no
	// other time, which is what makes a rotated token heal itself (§8).
	CredentialFile string
	Username       string
	SizeCeiling    int64
	CommitIdentity CommitIdentity
	LogLevel       slog.Level
}

// CommitIdentity is the git author obsync writes under — where provenance
// lives, and the reason the name rather than the address carries the meaning
// (§8).
type CommitIdentity struct {
	Name  string
	Email string
}

// Error is a config error: a refusal decidable from the configured values
// alone, and the only condition that makes obsync exit rather than park.
//
// It carries every problem in one environment block rather than the first,
// because an operator who finds them one at a time pays a redeploy for each.
type Error struct {
	Problems []error
}

func (e *Error) Error() string {
	problems := make([]string, len(e.Problems))
	for i, problem := range e.Problems {
		problems[i] = problem.Error()
	}
	return strings.Join(problems, "; ")
}

func (e *Error) Unwrap() []error { return e.Problems }

// Resolve reads the config surface out of an environment block, and returns it
// along with the logger the rest of the process writes to — whose level is the
// one thing §8 says about logging and §9 says the rest of.
//
// Everything Resolve has to say about that block it says through that logger,
// at the levels §9 fixes: an ERROR for a retired name, a WARN for an unknown
// one or a plain http:// remote, and the one INFO line echoing every resolved
// knob. A config error comes back as a value instead, because exiting belongs
// to the caller and nothing else in obsync may do it — and when one comes
// back, the Config is the zero value rather than a partly-resolved one, since
// a half-built config is exactly the valid-looking setting this design refuses
// to carry.
func Resolve(environ []string, stderr io.Writer) (Config, *slog.Logger, error) {
	set := block(environ)
	names := sortedNames(set)

	// The level is resolved first so that it is in force for every line below,
	// including the ones reporting the block it was read from. An unusable
	// level is still a config error; it just does not get to silence its own
	// diagnosis.
	level, levelErr := parseLevel(set[logLevelVar])
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	// Retired names are swept before unknown ones, and known() counts a
	// retired name as known, so a rename produces the ERROR that names its
	// replacement and never the WARN that would let an upgrade quietly revert
	// a setting to its default (§8).
	for _, name := range names {
		if replacement, isRetired := retired[name]; isRetired {
			log.Error("this variable was retired and is no longer read; the setting it carried has "+
				"reverted to its default", "variable", name, "replacement", replacement)
		}
	}
	for _, name := range names {
		if !known(name) {
			log.Warn("unknown variable, ignored", "variable", name)
		}
	}

	cfg := Config{
		VaultPath:      vaultPathIn(set),
		Branch:         set[branchVar],
		CredentialFile: set[tokenFileVar],
		Username:       valueOr(set, usernameVar, defaultUsername),
		SizeCeiling:    defaultSizeCeiling,
		CommitIdentity: CommitIdentity{
			Name:  valueOr(set, authorNameVar, defaultAuthorName),
			Email: valueOr(set, authorEmailVar, defaultAuthorEmail),
		},
		LogLevel: level,
	}

	// Every problem in the block, gathered in the order the surface declares
	// the variables, so an operator fixes them in one pass rather than one
	// redeploy at a time.
	var problems []error
	scheme := ""
	switch raw, ok := set[repoVar]; {
	case !ok:
		problems = append(problems, fmt.Errorf("%s is required: it is the one value nothing can "+
			"infer, and the only variable obsync cannot start without", repoVar))
	default:
		parsedScheme, remote, err := parseRemote(raw)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", repoVar, err))
		} else {
			cfg.RepoURL, cfg.ConfiguredRemote, scheme = raw, remote, parsedScheme
		}
	}

	// Plain http:// is warned about and never refused. This audience
	// self-hosts, and refusing would reject a real LAN deployment to protect
	// against a threat model its operator has already accepted (§8).
	if scheme == "http" {
		log.Warn("the remote is plain http://, so the credential and the vault cross the network "+
			"in the clear; obsync syncs it anyway", "remote", cfg.ConfiguredRemote)
	}

	if needsCredential(scheme) && cfg.CredentialFile == "" {
		problems = append(problems, fmt.Errorf("%s is required for an %s:// remote, which "+
			"authenticates with a credential rather than with a key", tokenFileVar, scheme))
	}
	if cfg.CredentialFile != "" {
		if err := readable(cfg.CredentialFile); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", tokenFileVar, err))
		}
	}

	if raw, ok := set[sizeCeilingVar]; ok {
		ceiling, err := parseSize(raw)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", sizeCeilingVar, err))
		} else {
			cfg.SizeCeiling = ceiling
		}
	}

	if levelErr != nil {
		problems = append(problems, fmt.Errorf("%s: %w", logLevelVar, levelErr))
	}

	if len(problems) > 0 {
		return Config{}, log, &Error{Problems: problems}
	}

	log.LogAttrs(context.Background(), slog.LevelInfo, "resolved configuration", cfg.attrs()...)
	return cfg, log, nil
}

// attrs renders the resolved config surface for the startup line: every knob,
// defaulted or set, so an operator can diff what obsync says it is running
// against what the declared surface says exists (§8).
//
// The token is absent — not redacted, not its first four characters: absent,
// so there is no prefix to leak and no "is that the real value?" question.
// That holds by construction rather than by filtering here, because obsync
// never reads the token at all outside a credential helper invocation, and
// this line is written from a Config that has only ever held its path.
func (c Config) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("repo", echoURL(c.RepoURL)),
		slog.String("remote", c.ConfiguredRemote.String()),
		slog.String("vault_path", c.VaultPath),
		slog.String("branch", c.Branch),
		slog.String("token_file", c.CredentialFile),
		slog.String("username", c.Username),
		slog.String("size_ceiling", FormatSize(c.SizeCeiling)),
		slog.String("author_name", c.CommitIdentity.Name),
		slog.String("author_email", c.CommitIdentity.Email),
		slog.String("log_level", strings.ToLower(c.LogLevel.String())),
	}
}

// CredentialEnvironment is the two variables a credential-helper invocation
// reads, resolved: the credential file's path and the username that goes
// beside it (§8).
//
// It is what obsync puts in the environment of every git it runs, because the
// helper git starts is obsync reading its own config surface in a process it
// did not resolve one for. Passing the resolved pair rather than letting the
// container's own block through means the helper answers with what this obsync
// is running on, and never with a value obsync itself refused.
func (c Config) CredentialEnvironment() []string {
	return []string{
		tokenFileVar + "=" + c.CredentialFile,
		usernameVar + "=" + c.Username,
	}
}

// CredentialFrom reads back what CredentialEnvironment wrote. The username
// keeps its default here as it does everywhere else on the config surface, so
// that a credential-helper invocation resolves the same value whoever started
// it.
func CredentialFrom(environ []string) (credentialFile, username string) {
	set := block(environ)
	return set[tokenFileVar], valueOr(set, usernameVar, defaultUsername)
}

// block reads the OBSYNC_* half of an environment block.
//
// An empty value is dropped rather than kept, so `OBSYNC_USERNAME=` in a
// compose file means the same as leaving the line out. The alternative is a
// deployment configured with the empty string, which is a valid-looking
// setting that is never what anyone meant.
func block(environ []string) map[string]string {
	set := make(map[string]string)
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, prefix) || value == "" {
			continue
		}
		set[name] = value
	}
	return set
}

func sortedNames(set map[string]string) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// known reports whether obsync recognises a name at all. A retired name counts
// as known, so it gets the ERROR naming its replacement and never also the
// WARN for a name obsync has never had.
func known(name string) bool {
	if slices.Contains(knobs, name) {
		return true
	}
	_, isRetired := retired[name]
	return isRetired
}

// VaultPath is where the vault is, read out of an environment block on its own
// and without resolving anything else.
//
// It exists for the two signal subcommands (§9), which need the vault and
// nothing else about the configuration: they answer *does this need a human?*
// out of obsync's own record of itself, and Resolve would make that answer wait
// on a question it is not asking. Specifically, Resolve stats the credential
// file, and §8 is explicit that a token file turning unreadable *later* is not
// a config error but the self-healing bad-credential tier — so a healthcheck
// built on Resolve would report a running, healthy obsync as unhealthy for the
// length of a token rotation, and invite the restart that turns it into a crash
// loop.
//
// Nothing is lost by not checking: a configuration obsync could not use is one
// obsync exited on, so it wrote no status file, and the absent-or-stale file
// says so already (§9).
//
// It shares vaultPathIn with Resolve rather than restating the default, because
// two spellings of where the vault is are two answers waiting to drift.
func VaultPath(environ []string) string {
	return vaultPathIn(block(environ))
}

func vaultPathIn(set map[string]string) string {
	return valueOr(set, vaultPathVar, defaultVaultPath)
}

func valueOr(set map[string]string, name, fallback string) string {
	if value, ok := set[name]; ok {
		return value
	}
	return fallback
}

// needsCredential reports whether a remote of this scheme authenticates with a
// credential file. Only http(s) does: ssh takes a key and file:// takes
// nothing, which is why OBSYNC_TOKEN_FILE is required iff the URL is
// http(s):// and why SSH needs no knobs at all (§8).
func needsCredential(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// readable reports whether obsync's UID can read a file, without reading it —
// the secret is git's to ask for, not startup's to look at (§8).
//
// A credential file configured but unreadable at startup is a config error;
// the same file turning unreadable later is not, it is the self-healing
// bad-credential tier (§7, §8). Anything that is not a regular file is refused
// as well as an unreadable one, because opening a directory succeeds and
// reading a token out of it never will, and a compose file naming the mount
// point rather than the file inside it is how that happens.
//
// The shape check comes before the open, and that order is load-bearing rather
// than tidy: opening a FIFO blocks until a writer appears, and it would block
// here with the SIGTERM handler already installed and nothing yet reading it —
// a container that never starts, says nothing, and cannot be stopped without
// SIGKILL, out of one wrong path in a compose file. A regular file is the one
// shape whose open cannot block, so obsync opens nothing else.
func readable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot be read: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a file, so no credential can be read from it", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot be read: %w", err)
	}
	// Nothing was read, so nothing is lost by dropping the close error; it is
	// dropped explicitly rather than by omission.
	_ = file.Close()
	return nil
}
