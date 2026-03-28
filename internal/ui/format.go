package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// FormatSize converts bytes to human-readable format.
func FormatSize(sizeBytes *int64) string {
	if sizeBytes == nil {
		return "?"
	}
	return FormatBytes(*sizeBytes)
}

// FormatBytes converts bytes to human-readable format.
func FormatBytes(b int64) string {
	if b == 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	val := float64(b) / float64(div)
	units := []string{"KB", "MB", "GB", "TB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return fmt.Sprintf("%.1f %s", val, units[exp])
}

// QualityIndicator returns a colored emoji for quality score (0-100 scale).
func QualityIndicator(score *int) string {
	if score == nil {
		return "  "
	}
	s := *score
	switch {
	case s >= 70:
		return "🟢"
	case s >= 40:
		return "🟡"
	default:
		return "🔴"
	}
}

// SeedHealthIndicator returns a colored emoji for seed count.
func SeedHealthIndicator(seeds int) string {
	switch {
	case seeds > 100:
		return "🟢"
	case seeds >= 10:
		return "🟡"
	default:
		return "🔴"
	}
}

// FormatRating returns a display string for a rating.
func FormatRating(rating *string) string {
	if rating == nil {
		return "-"
	}
	return *rating
}

// FormatYear returns a display string for a year.
func FormatYear(year *int) string {
	if year == nil {
		return "-"
	}
	return strconv.Itoa(*year)
}

// FormatContentType returns a short display for content type.
func FormatContentType(ct string) string {
	switch strings.ToLower(ct) {
	case "movie":
		return "Movie"
	case "show":
		return "Show"
	default:
		return ct
	}
}

// Ptr returns a pointer to a value. Useful for tests.
func Ptr[T any](v T) *T {
	return &v
}

// TruncateString truncates a string to maxLen with ellipsis.
func TruncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

// FormatLanguages joins language codes.
func FormatLanguages(langs []string) string {
	if len(langs) == 0 {
		return "-"
	}
	return strings.Join(langs, ", ")
}

// FormatSeedRatio returns a display for seed/leech ratio.
func FormatSeedRatio(seeders, leechers int) string {
	if leechers == 0 {
		if seeders == 0 {
			return "0:0"
		}
		return fmt.Sprintf("%d:0", seeders)
	}
	ratio := float64(seeders) / float64(leechers)
	return fmt.Sprintf("%.0f:1", math.Round(ratio))
}

// FormatTimeAgo returns a human-readable "time ago" string.
func FormatTimeAgo(t string) string {
	parsed, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return t
	}
	diff := time.Since(parsed)
	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		m := int(diff.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case diff < 24*time.Hour:
		h := int(diff.Hours())
		return fmt.Sprintf("%dh ago", h)
	case diff < 30*24*time.Hour:
		d := int(diff.Hours() / 24)
		return fmt.Sprintf("%dd ago", d)
	default:
		m := int(diff.Hours() / 24 / 30)
		return fmt.Sprintf("%dmo ago", m)
	}
}

// FormatNumber formats a number with thousands separator.
func FormatNumber(n int) string {
	negative := n < 0
	if negative {
		n = -n
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		if negative {
			return "-" + s
		}
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	if negative {
		return "-" + string(result)
	}
	return string(result)
}

// StringOrDash returns the string value or "-" if nil.
func StringOrDash(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}
