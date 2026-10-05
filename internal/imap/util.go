package imap

import (
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/peltonapp/Pelton/internal/charsetguess"
	"github.com/peltonapp/Pelton/internal/rfc822"
)

// formatAddresses renders an address list as `Name <user@host>, ...` through
// rfc822.FormatAddress, so it is stored in the same shape as a parsed body. A
// display name the server handed over as raw 8-bit bytes is decoded here, since
// an envelope carries no charset of its own to convert it by.
func formatAddresses(addrs []imap.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if s := rfc822.FormatAddress(a.Name, a.Addr()); s != "" {
			parts = append(parts, s)
		}
	}
	joined, _ := charsetguess.Text(strings.Join(parts, ", "))
	return joined
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
