package store

import (
	"fmt"
	"strings"
)

// lastPathComponent returns the final path element, handling both / and \
// separators and trailing slashes.
func lastPathComponent(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// HumanSize renders a byte count in binary units (KiB/MiB/…), like a file
// manager. Kept here so callers don't reach into internal formatting.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
