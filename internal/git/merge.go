package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andyroberts2/obsync/internal/config"
)

// The out-of-tree merge (§4).
//
// "Keep both sides" is implemented so that a conflicted state never exists in
// the vault at all: `merge-tree --write-tree` computes the whole merge with the
// working tree untouched, obsync substitutes its own resolution per conflicted
// path, `commit-tree`s the result with both parents, and applies that in one
// step with `reset --keep`. The tree merge-tree hands back holds conflict
// markers at every content-conflicted path, so it is never checked out as it
// stands — which is the whole reason the computing half is a pure function of
// two commits and the applying half is one command at the end.
//
// This is what fixes the git floor at 2.38, where --write-tree landed. Every
// measurement behind this file was taken on 2.38.5 and on 2.52.0, and the two
// agree.
//
// `git merge -X ours` and a custom merge driver are both rejected here rather
// than merely unused: each intercepts only *content* conflicts, so a
// modify/delete, a rename/rename or an add/add still stops the merge with an
// unmerged index — a conflicted working tree under a live Obsidian, which is
// the one state this policy exists to forbid.

// ConflictCopy is the losing side of a conflicted path, kept beside it (§4).
//
// The vault's view of the canonical path wins — including absence, where the
// vault deleted it — and these are the bytes that would otherwise have been
// discarded. A human resolves one by editing the two notes together and
// deleting the copy, which the ordinary loop commits like any other edit: the
// filename is the whole of the state, so there is nothing to reconcile after a
// crash and no command to remember.
type ConflictCopy struct {
	// Path is where the copy landed and Of is the canonical path it is a copy
	// of — the two names a human edits together. Both are vault-relative and in
	// git's own spelling.
	Path string
	Of   string
}

// conflictMarker is the fixed part of a conflict copy's name, and it is
// deliberately cheap to recognise: the pattern *is* the recovery state, so a
// glob over the vault is the only thing that ever has to know a conflict exists
// (§4).
//
// It avoids every character Obsidian forbids in a filename — #, ^, [, ] and | —
// and colons, which Obsidian allows and not every filesystem a vault may sit on
// does.
const conflictMarker = " (obsync conflict "

// conflictStamp is UTC at minute precision: as fine as a name a human reads
// should be, and coarse enough that two copies of one note inside a minute
// collide rather than looking like two different conflicts. The collision is
// answered by a counter, and never by an overwrite.
const conflictStamp = "2006-01-02 1504"

// merge is §4's whole act: compute the merge outside the vault, resolve every
// conflicted path by the keep-both rule, commit it with both parents, and apply
// it. It answers with the conflict copies that commit carries.
//
// Nothing here writes the vault until the last step, and the last step is one
// `reset --keep`. A run abandoned before it — because the vault is being
// written where the merge lands — leaves an unreachable commit behind for gc
// and nothing else; the next run recomputes the merge against the new HEAD,
// which costs nothing precisely because the computing half touched nothing.
func (r *Repo) merge(tip string) ([]ConflictCopy, error) {
	merged, conflicted, said, err := r.mergeTree(tip)
	if err != nil {
		return nil, err
	}

	tree, copies, err := r.resolved(merged, conflicted, said)
	if err != nil {
		return nil, err
	}
	commit, err := r.commitMerge(tree, tip)
	if err != nil {
		return nil, err
	}
	if err := r.applyMerge(commit); err != nil {
		return nil, err
	}
	return copies, nil
}

// mergeTreeConflicted is the status `merge-tree --write-tree` exits with when
// it merged both sides and some paths conflicted. git-merge-tree(1) documents 0
// and 1 as answers and anything else as an error, so this is a closed enum in
// an exit status — the same kind of fact `merge-base --is-ancestor` answers
// with, and the reason obsync never reads a word of what merge-tree writes for
// a human.
const mergeTreeConflicted = 1

// mergeTree computes the merge of the vault's HEAD and the remote's tip without
// touching the working tree, and reads what git answered with.
//
// HEAD rather than the tracked branch by name, for classify's reason: HEAD is
// what the apply moves and what the push sends, and the loop refuses to act at
// all when HEAD is not on the tracked branch.
func (r *Repo) mergeTree(tip string) (tree string, conflicted []conflictedEntry, said conflictReport, err error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"merge-tree", "--write-tree", "-z", "HEAD", tip},
	})
	var command *CommandError
	if errors.As(err, &command) && command.ExitCode == mergeTreeConflicted {
		out, err = command.Stdout, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	return parseMergeTree(out)
}

// conflictedEntry is one stage of one conflicted path: the blob that side of
// the merge holds there, and the mode it holds it in.
//
// Stage 1 is the merge base, stage 2 the vault, stage 3 the remote — git's own
// numbering, and the reason the keep-both rule is expressible without reading a
// word of prose: which of 2 and 3 exists says which side has bytes at the path,
// and "the vault's view wins, the remote's losing bytes become a copy" is
// exactly a statement about those two.
type conflictedEntry struct {
	mode  string
	oid   string
	stage int
	path  string
}

// conflictReport is what git said about each path it had trouble with: the kind
// of trouble, and the other paths the same message was about.
//
// The kind is the machine half of git's message pair and the message itself is
// the human half — measured rather than assumed: git's translation catalogue
// carries the messages, with their format specifiers, and carries none of the
// bare kind strings. So obsync branches on the kind and never on the message,
// which is the same split `push --porcelain` has (§1).
//
// The companion paths are load-bearing for exactly one row. When git resolves a
// file/directory conflict itself it renames the losing file out of the way and
// says so in a message naming both the new name and the canonical one, so the
// canonical path is something git states rather than something obsync recovers
// from the suffix it happens to have chosen.
type conflictReport map[string][]conflictSaid

type conflictSaid struct {
	kind string
	// paths is every path this message named, including the one it is filed
	// under.
	paths []string
}

// parseMergeTree reads `merge-tree --write-tree -z`.
//
// The output is the merged tree's object name, then — when anything conflicted
// — an empty record, the conflicted file info, another empty record, and the
// informational messages. Measured at both matrix points: a clean merge is the
// object name and nothing else, and with -z a conflicted-file-info record is
// `<mode> <oid> <stage>\t<path>` while a message is a path count, that many
// paths, the conflict kind, and the message itself.
//
// Records are NUL-separated because a note title may legally contain a newline,
// and within a record the path is what follows the first tab: nothing before it
// can hold one.
func parseMergeTree(out []byte) (tree string, conflicted []conflictedEntry, said conflictReport, err error) {
	records := splitNUL(out)
	if len(records) == 0 {
		return "", nil, nil, errors.New("git merge-tree --write-tree answered nothing")
	}
	tree = records[0]
	said = conflictReport{}

	at := 1
	for ; at < len(records) && records[at] != ""; at++ {
		entry, err := parseConflictedEntry(records[at])
		if err != nil {
			return "", nil, nil, err
		}
		conflicted = append(conflicted, entry)
	}
	at++

	for at < len(records) {
		count, err := strconv.Atoi(records[at])
		if err != nil || count < 0 {
			return "", nil, nil, fmt.Errorf("git merge-tree reported a message obsync could not "+
				"read: %q is not a count of paths", records[at])
		}
		at++
		// The paths, the kind and the message. The message itself is read only
		// so that it is not mistaken for the next record.
		if at+count+1 >= len(records) {
			return "", nil, nil, fmt.Errorf("git merge-tree reported a message about %d paths with "+
				"fewer than that left to read", count)
		}
		message := conflictSaid{kind: records[at+count], paths: records[at : at+count]}
		at += count + 2
		for _, relative := range message.paths {
			said[relative] = append(said[relative], message)
		}
	}
	return tree, conflicted, said, nil
}

// parseConflictedEntry reads one `<mode> <oid> <stage>\t<path>` record.
func parseConflictedEntry(record string) (conflictedEntry, error) {
	unreadable := func() (conflictedEntry, error) {
		return conflictedEntry{}, fmt.Errorf("git merge-tree reported a conflicted path obsync "+
			"could not read: %q", record)
	}
	mode, rest, found := strings.Cut(record, " ")
	if !found {
		return unreadable()
	}
	oid, rest, found := strings.Cut(rest, " ")
	if !found {
		return unreadable()
	}
	stage, relative, found := strings.Cut(rest, "\t")
	if !found {
		return unreadable()
	}
	number, err := strconv.Atoi(stage)
	if err != nil {
		return conflictedEntry{}, fmt.Errorf("git merge-tree reported the stage %q, which is not a "+
			"number, for %q", stage, relative)
	}
	return conflictedEntry{mode: mode, oid: oid, stage: number, path: relative}, nil
}

// The conflict kinds §4's closed table is written in terms of, spelled as git
// spells them. Everything else is outside the table.
const (
	// kindContents is a path both sides hold different bytes at — the content
	// row, and the add/add row, which §4 answers identically.
	kindContents = "CONFLICT (contents)"
	// kindModifyDelete is one side editing a path the other removed. Which
	// side did which is the stages' answer rather than this one's, and it is
	// what tells §4's modify/delete row from its delete/modify row.
	kindModifyDelete = "CONFLICT (modify/delete)"
	// kindRenameRename is one path renamed to two different names. Both names
	// already exist in the merged tree, each holding its own side's content,
	// which is exactly what the table asks for — so this row's resolution is
	// to leave git's alone.
	kindRenameRename = "CONFLICT (rename/rename)"
	// kindFileDirectory is a file on one side where the other has a directory.
	// git resolves it itself by renaming the file out of the way; obsync keeps
	// that resolution and renames git's suffix into its own convention.
	kindFileDirectory = "CONFLICT (file/directory)"
	// kindBinary and kindAutoMerging are not rows. They accompany one: git says
	// it could not merge a binary and then reports the ordinary content
	// conflict beside it, which is precisely why §4 gives binaries no special
	// casing at all.
	kindBinary      = "CONFLICT (binary)"
	kindAutoMerging = "Auto-merging"
)

// resolved is the keep-both rule applied to every conflicted path, and answers
// with the tree obsync will commit plus the copies it carries.
//
// The rule is one rule and it is not configurable: **the vault's view of the
// canonical path wins — including absence — and the remote's losing bytes
// become a conflict copy.** The kind says which of §4's rows a path is on, and
// the stages say which side of it the vault is.
//
// A path that needs nothing is not made into a record: a clean line-level merge
// is kept exactly as git computed it, so two devices appending to one daily
// note — the common case — produces no copy at all, and neither binaries nor
// `.obsidian/` config get any special casing on the way through.
func (r *Repo) resolved(merged string, conflicted []conflictedEntry,
	said conflictReport) (string, []ConflictCopy, error) {

	var records bytes.Buffer
	var copies []ConflictCopy
	taken := map[string]bool{}

	for _, at := range conflictedPaths(conflicted) {
		kinds := said[at.path]
		if err := insideTheTable(at.path, kinds); err != nil {
			return "", nil, err
		}
		ours, hasOurs := at.stages[2]
		theirs, hasTheirs := at.stages[3]

		switch {
		case names(kinds, kindFileDirectory):
			// git has already put a directory at the canonical path and moved
			// the losing file aside under a suffix of its own. §4 keeps that
			// resolution — a tree must hold one thing at a path, and both
			// sides' bytes survive it — and renames the suffix into obsync's
			// own convention, because `note.md~HEAD` is a name from git's argv
			// rather than one that means anything in a vault.
			//
			// Exactly one side is moved — the one that is a file where the
			// other is a directory — so exactly one stage is reported for the
			// new name, and which it is says whose bytes they are. Measured at
			// both matrix points, in both directions.
			if !hasOurs && !hasTheirs {
				return "", nil, fmt.Errorf("%w: git renamed %q out of the way and reported neither "+
					"side's blob for it", ErrConflictOutsideTheTable, at.path)
			}
			losing := ours
			if hasTheirs {
				losing = theirs
			}
			canonical, err := renamedOutOfTheWay(at.path, kinds)
			if err != nil {
				return "", nil, err
			}
			written, err := r.copyOf(merged, canonical, taken)
			if err != nil {
				return "", nil, err
			}
			removeFromIndex(&records, at.path, losing.oid)
			setInIndex(&records, conflictedEntry{mode: losing.mode, oid: losing.oid, path: written.Path})
			copies = append(copies, written)

		case names(kinds, kindRenameRename):
			// Both names exist in the merged tree, each with its own side's
			// content, and the content rule is already applied at the vault's
			// name. There is nothing to substitute and no copy to write: the
			// remote's bytes are not losing, they are at a name of their own.

		case hasOurs && hasTheirs:
			// The content row, and the add/add row with it. The vault's bytes
			// go back to the canonical path — git left conflict markers there,
			// and this substitution is why one never reaches a note — and the
			// remote's become a copy beside it.
			written, err := r.copyOf(merged, at.path, taken)
			if err != nil {
				return "", nil, err
			}
			setInIndex(&records, ours)
			setInIndex(&records, conflictedEntry{mode: theirs.mode, oid: theirs.oid, path: written.Path})
			copies = append(copies, written)

		case hasOurs:
			// modify/delete: the vault edited a path the remote removed. The
			// file stays and there is no copy, because the remote has no bytes
			// at this path to preserve. git already leaves the vault's version
			// in the merged tree; obsync writes it anyway, so that what the
			// commit holds is what obsync's rule decided rather than what git
			// happened to do.
			setInIndex(&records, ours)

		case hasTheirs:
			// delete/modify: the vault deleted a path the remote edited. The
			// deletion stands — absence is a view of the path like any other —
			// and the remote's version, which git left sitting at the canonical
			// path, becomes a copy instead.
			written, err := r.copyOf(merged, at.path, taken)
			if err != nil {
				return "", nil, err
			}
			removeFromIndex(&records, at.path, theirs.oid)
			setInIndex(&records, conflictedEntry{mode: theirs.mode, oid: theirs.oid, path: written.Path})
			copies = append(copies, written)

		default:
			// Only the merge base holds this path: it is where a rename came
			// from, and the merged tree has already stopped carrying it.
		}
	}

	if records.Len() == 0 {
		return merged, copies, nil
	}
	tree, err := r.treeWith(merged, records.Bytes())
	return tree, copies, err
}

// conflictedPath is one path and every stage of it git reported, in the order
// git first named it — so that the copies a merge writes are named in the same
// order on every run rather than in whichever order a map handed them back.
type conflictedPath struct {
	path   string
	stages map[int]conflictedEntry
}

func conflictedPaths(conflicted []conflictedEntry) []conflictedPath {
	var grouped []conflictedPath
	at := map[string]int{}
	for _, entry := range conflicted {
		index, known := at[entry.path]
		if !known {
			index = len(grouped)
			at[entry.path] = index
			grouped = append(grouped, conflictedPath{path: entry.path, stages: map[int]conflictedEntry{}})
		}
		grouped[index].stages[entry.stage] = entry
	}
	return grouped
}

// names reports whether git said this kind of thing about the path.
func names(kinds []conflictSaid, kind string) bool {
	for _, said := range kinds {
		if said.kind == kind {
			return true
		}
	}
	return false
}

// insideTheTable refuses a conflicted path §4's table has no row for, and it is
// asked of every kind git named the path in rather than only of the one the
// dispatch below happens to match — so a shape obsync recognises in part is
// refused rather than resolved by the half it recognised.
//
// The table is closed, and closing it is the decision rather than an omission:
// submodules, a symlink against a file, and whatever a future git adds are all
// cases where obsync would have to invent a resolution, and inventing one is
// how bytes get lost quietly. Stopping instead is the fail-closed-outward rule
// — the vault is sound and only its relationship to the remote is not.
//
// Two of the kinds allowed here are not rows and are not resolved. They
// accompany a row: git says it could not merge a binary, or that it merged
// content at the path, and neither changes what happens to it. §4 says so in as
// many words — binaries get no special casing, because git reports the plain
// content conflict beside the binary one.
func insideTheTable(relative string, kinds []conflictSaid) error {
	for _, said := range kinds {
		switch said.kind {
		case kindContents, kindModifyDelete, kindRenameRename, kindFileDirectory,
			kindBinary, kindAutoMerging:
		default:
			return fmt.Errorf("%w: git reported %q at %q", ErrConflictOutsideTheTable, said.kind, relative)
		}
	}
	return nil
}

// renamedOutOfTheWay is the canonical path a file/directory conflict is about,
// given the name git moved the losing file to.
//
// It is the other path git's own message named, rather than the moved name with
// its suffix trimmed off. The suffix is whatever obsync passed on the argv —
// measured at both matrix points, `note.md~HEAD` against HEAD and
// `note.md~<oid>` against the remote's tip — so trimming it would work and
// would be obsync reconstructing something git already stated. A message that
// does not name exactly the two paths is one obsync does not recognise, and an
// unrecognised shape is refused rather than guessed at.
func renamedOutOfTheWay(moved string, kinds []conflictSaid) (string, error) {
	for _, said := range kinds {
		if said.kind != kindFileDirectory || len(said.paths) != 2 {
			continue
		}
		for _, relative := range said.paths {
			if relative != moved {
				return relative, nil
			}
		}
	}
	return "", fmt.Errorf("%w: git renamed %q out of the way without naming what it was in the way "+
		"of", ErrConflictOutsideTheTable, moved)
}

// copyOf is where the losing side's bytes go, and it guards the one way this
// design could actually lose some: **an existing copy is never overwritten.**
//
// A name is only taken when nothing holds it — not the tree obsync is about to
// commit, not the vault on disk, and not another copy this same merge is
// already writing. A taken name gets a counter rather than a replacement, and
// the loop cannot run away: every candidate that is taken stays taken, so the
// first free one is reached and kept.
func (r *Repo) copyOf(merged, canonical string, taken map[string]bool) (ConflictCopy, error) {
	for counter := 1; ; counter++ {
		candidate := conflictCopyName(canonical, r.clock.Now(), counter)
		if taken[candidate] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(r.vault, filepath.FromSlash(candidate))); err == nil {
			continue
		}
		held, err := r.treeHolds(merged, candidate)
		if err != nil {
			return ConflictCopy{}, err
		}
		if held {
			continue
		}
		taken[candidate] = true
		return ConflictCopy{Path: candidate, Of: canonical}, nil
	}
}

// conflictCopyName is the name the losing side lands under: the canonical name
// with a marker before its extension, in the same folder (§4).
//
// The extension is preserved and stays last, which is what makes Obsidian
// render the copy as a note and link it — a copy the human cannot open is a
// copy they cannot resolve. A collision inside the same minute appends a
// counter.
//
// The bytes are not touched anywhere in this file: no injected frontmatter and
// no provenance header. Annotating a copy is the same class of act as leaving
// conflict markers in one, and it would corrupt the copy's own frontmatter
// besides.
func conflictCopyName(canonical string, at time.Time, counter int) string {
	folder, name := path.Split(canonical)
	extension := path.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	if stem == "" {
		// A dotfile is all extension as far as path.Ext is concerned, and a
		// name that is nothing but a marker and a suffix is not the name of
		// anything a human would recognise.
		stem, extension = name, ""
	}

	marker := conflictMarker + at.UTC().Format(conflictStamp) + ")"
	if counter > 1 {
		marker += " " + strconv.Itoa(counter)
	}
	return folder + stem + marker + extension
}

// treeHolds reports whether a tree already carries a path.
//
// ls-tree with a pathspec rather than `cat-file -e <tree>:<path>`, and the
// difference is the whole reason: measured at both matrix points, cat-file
// exits 128 for a path a tree does not hold — git's everything-code, which also
// covers a damaged object store — while ls-tree exits 0 either way and answers
// by printing the path or printing nothing. One is a conclusive fact and the
// other is a guess.
//
// The pathspec is :(literal) for Stage's reason: a note title may hold a glob
// character, and `Notes/[draft] plan.md` is a name rather than a character
// class.
func (r *Repo) treeHolds(tree, relative string) (bool, error) {
	out, err := r.run(invocation{
		dir:  r.vault,
		args: []string{"ls-tree", "-z", "--name-only", tree, "--", ":(literal)" + relative},
	})
	if err != nil {
		return false, err
	}
	return len(splitNUL(out)) > 0, nil
}

// setInIndex and removeFromIndex write one record of what `git update-index
// --index-info` reads: `<mode> <oid>\t<path>`, with mode 0 meaning the path goes
// away. NUL-separated, because a note title may contain a newline.
//
// The null object name is spelled at the length of an object name git has just
// handed obsync rather than at a fixed forty characters, so a repository whose
// objects are not SHA-1 is read with its own hash rather than with an
// assumption about one.
func setInIndex(records *bytes.Buffer, entry conflictedEntry) {
	fmt.Fprintf(records, "%s %s\t%s", entry.mode, entry.oid, entry.path)
	records.WriteByte(0)
}

func removeFromIndex(records *bytes.Buffer, relative, oidOfKnownLength string) {
	fmt.Fprintf(records, "0 %s\t%s", strings.Repeat("0", len(oidOfKnownLength)), relative)
	records.WriteByte(0)
}

// treeWith is the merged tree with obsync's own resolution substituted into it.
//
// It is built through an index because that is the only thing that builds a
// tree, and through a *temporary* one because the vault's index is shared with
// whatever else runs git in the vault: a human's staged work is theirs, and
// obsync computing a merge is not a reason to touch it. The temporary index
// lives in obsync's staging directory, which is an owned path inside .git and
// is swept at startup, so a crash here leaves debris nothing reads rather than
// an index somebody else has to notice.
func (r *Repo) treeWith(base string, records []byte) (string, error) {
	if err := os.MkdirAll(r.staging, 0o755); err != nil {
		return "", fmt.Errorf("obsync could not make room to compute the merge: %w", err)
	}
	index, err := os.CreateTemp(r.staging, "merge-index.*")
	if err != nil {
		return "", fmt.Errorf("obsync could not make room to compute the merge: %w", err)
	}
	if err := index.Close(); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(index.Name()) }()
	env := []string{"GIT_INDEX_FILE=" + index.Name()}

	if _, err := r.run(invocation{dir: r.vault, env: env, args: []string{"read-tree", base}}); err != nil {
		return "", err
	}
	if _, err := r.run(invocation{
		dir: r.vault, env: env, stdin: records,
		args: []string{"update-index", "-z", "--index-info"},
	}); err != nil {
		return "", err
	}
	out, err := r.run(invocation{dir: r.vault, env: env, args: []string{"write-tree"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// commitMerge records the resolved tree as a merge commit with both parents.
//
// The message is git's own default and obsync composes none of it: `git merge`
// writes a description of what it is merging and hands that to
// `fmt-merge-msg`, so obsync hands the same description to the same command and
// gets the same sentence — including whatever the vault's own config asks of it,
// which is the human's file and outranks obsync's. A message obsync invented
// here would be one more thing to keep in step with git for no gain, since
// provenance lives in the commit identity, which stays obsync's.
//
// The first parent is the vault, which is what makes `git log --first-parent`
// in the vault read as the vault's own history.
func (r *Repo) commitMerge(tree, tip string) (string, error) {
	upstream := config.RemoteName + "/" + r.branch
	message, err := r.run(invocation{
		dir:   r.vault,
		stdin: []byte(tip + "\t\tremote-tracking branch '" + upstream + "'\n"),
		args:  []string{"fmt-merge-msg", "-F", "-"},
	})
	if err != nil {
		return "", err
	}
	out, err := r.run(invocation{
		dir:   r.vault,
		stdin: message,
		args:  []string{"commit-tree", tree, "-p", "HEAD", "-p", tip, "-F", "-"},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// applyMerge puts the merge in the vault, and is the one moment in §4 that
// writes a file a human owns.
//
// `reset --keep` moves the branch and updates the working tree in one step, and
// updates only the paths that differ — so a note the human is editing that the
// merge does not touch is left exactly where it is. It is never forced: obsync
// checks first that the vault is not being written where the merge lands, and a
// vault that is abandons the run rather than having anything overwritten.
// Nothing about that is reported above debug, because the next run recomputes
// the merge against the new HEAD and applies it, and the commit this one built
// is left unreachable for gc.
func (r *Repo) applyMerge(commit string) error {
	touched, err := r.pathsBetween("HEAD", commit)
	if err != nil {
		return err
	}
	if err := r.refuseWhileTheVaultIsWritten(touched); err != nil {
		return err
	}
	_, err = r.run(invocation{dir: r.vault, args: []string{"reset", "--keep", "--quiet", commit}})
	return err
}

// ErrConflictOutsideTheTable is a conflict §4's closed table has no row for.
//
// It is a network freeze (§7): the vault is sound, its relationship to the
// remote is not, and the local half keeps committing while a human looks. What
// obsync will not do is improvise a resolution — every row of the table is a
// rule about which side's bytes survive, and a kind with no row is one where
// obsync does not know the answer. Sorting it into that tier is #31's, along
// with the two merge ceilings that stop the network half the same way.
var ErrConflictOutsideTheTable = errors.New("the merge hit a conflict obsync has no rule for")
