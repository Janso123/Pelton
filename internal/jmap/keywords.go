package jmap

import "github.com/peltonapp/Pelton/internal/storage"

// FlagsFromKeywords maps JMAP keywords onto storage.Flag. Only $seen and
// $flagged are recognized; deletion is a separate Email/set destroy or move.
func FlagsFromKeywords(kw map[string]bool) storage.Flag {
	var f storage.Flag
	if kw["$seen"] {
		f |= storage.FlagSeen
	}
	if kw["$flagged"] {
		f |= storage.FlagFlagged
	}
	return f
}

// KeywordsFromFlags maps storage.Flag onto JMAP keywords. FlagDeleted is never
// mapped to a keyword.
func KeywordsFromFlags(f storage.Flag) map[string]bool {
	out := make(map[string]bool)
	if f.Has(storage.FlagSeen) {
		out["$seen"] = true
	}
	if f.Has(storage.FlagFlagged) {
		out["$flagged"] = true
	}
	return out
}
