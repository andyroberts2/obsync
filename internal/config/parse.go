package config

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
)

// sizeUnits are the suffixes a size may carry, longest first so that GB is not
// read as a G followed by B.
//
// The multipliers are powers of 1024, which is what git's own config sizes
// mean, so a number an operator copies out of a git setting means the same
// thing here. That also puts the 95MB default under GitHub's 100MB hard block
// on either reading of "MB".
var sizeUnits = []struct {
	suffix string
	scale  int64
}{
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"KB", 1 << 10},
	{"B", 1},
}

// parseSize reads a size the way a human writes one — a whole number and a
// unit, 95MB — and never as raw bytes (§8). A bare number is refused rather
// than guessed at: 104857600 and 100MB are the same size, and only one of them
// is a value an operator can check at a glance.
func parseSize(raw string) (int64, error) {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	for _, unit := range sizeUnits {
		digits, ok := strings.CutSuffix(upper, unit.suffix)
		if !ok {
			continue
		}
		size, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || size < 1 || size > math.MaxInt64/unit.scale {
			return 0, fmt.Errorf("%q is not a size: it takes a whole number of %s, as in 95MB", raw, unit.suffix)
		}
		return size * unit.scale, nil
	}
	return 0, fmt.Errorf("%q carries no unit: a size takes a human suffix — B, KB, MB or GB, as in "+
		"95MB — and never raw bytes", raw)
}

// formatSize writes a size back in the form it was configured in, in the
// largest unit that divides it exactly.
func formatSize(size int64) string {
	for _, unit := range sizeUnits {
		if size%unit.scale == 0 {
			return strconv.FormatInt(size/unit.scale, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(size, 10) + "B"
}

// parseLevel reads OBSYNC_LOG_LEVEL. The four levels are §9's closed list, and
// they have fixed meanings there: ERROR means a human is needed, WARN means
// true but self-healing, INFO is the startup line and runs that changed
// something, DEBUG is every git invocation.
//
// An unusable level is a config error, and comes back with the default anyway
// so that the diagnosis it caused can still be logged.
func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "":
		return defaultLogLevel, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return defaultLogLevel, fmt.Errorf("%q is not a log level: obsync logs at debug, info, warn or "+
		"error", raw)
}
