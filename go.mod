// obsync is stdlib plus exactly two direct dependencies: a filesystem-
// notification library, and golang.org/x/sys for flock and statfs. Logging is
// log/slog; configuration is os.Getenv and hand-written parsing, because there
// are no flags and no config file to need a library for (spec #21 §1, §8).
//
// A third direct dependency is not forbidden, it is argued for: the commit that
// adds it says what stdlib could not do. This is the supply chain of a
// container holding a write-scoped credential, so indirect dependencies count
// too — a library that drags in five is five — and test-only dependencies are
// held to the same bar.
//
// There is deliberately no `toolchain` directive here: it triggers a toolchain
// download, which makes a pinned release builder not pinned (§12).
module github.com/andyroberts2/obsync

go 1.25.0

require github.com/fsnotify/fsnotify v1.10.1

require golang.org/x/sys v0.47.0
