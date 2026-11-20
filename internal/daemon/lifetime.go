package daemon

import (
	"fmt"
	"strconv"
	"time"

	"github.com/onrcanogul/liman/internal/store"
)

// maxLifetimeDays caps --expires. A token meant to live longer than this
// should simply not expire; the cap also keeps the arithmetic far away from
// time.Duration's limit of about 292 years.
const maxLifetimeDays = 3650

// parseLifetime reads a token lifetime such as "90d" or "12h".
//
// time.ParseDuration is not used: it knows no unit larger than hours, and
// "2160h" is not something anybody should have to type for three months.
// Mixed values ("1d12h") are not accepted either; they are rare enough for
// a lifetime that a simpler rule is worth more.
func parseLifetime(s string) (time.Duration, error) {
	invalid := fmt.Errorf("--expires %q: use a whole number of days or hours, such as 90d or 12h", s)
	if len(s) < 2 {
		return 0, invalid
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	// Atoi also accepts "+5" and "-5"; only plain digits are meant here.
	if err != nil || n <= 0 || s[0] < '0' || s[0] > '9' {
		return 0, invalid
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 'd':
		unit = 24 * time.Hour
	case 'h':
		unit = time.Hour
	default:
		return 0, invalid
	}
	if time.Duration(n)*unit > maxLifetimeDays*24*time.Hour {
		return 0, fmt.Errorf("--expires %q: at most %d days; leave --expires out for a token that does not expire", s, maxLifetimeDays)
	}
	return time.Duration(n) * unit, nil
}

// describeExpiry says when a token expires, for listings: "never",
// "expired" or the local time with how long is left.
func describeExpiry(t store.TokenInfo, now time.Time) string {
	switch {
	case t.ExpiresAt.IsZero():
		return "never"
	case t.Expired(now):
		return "expired " + t.ExpiresAt.Local().Format("2006-01-02 15:04")
	default:
		return t.ExpiresAt.Local().Format("2006-01-02 15:04") + " (in " + roughly(t.ExpiresAt.Sub(now)) + ")"
	}
}

// roughly gives a duration as a person would say it: "90 days", "47
// hours", "12 minutes". Days and hours are rounded to the nearest, so that
// a token created for 5 days is not listed as "in 4 days" a second later;
// a unit is only used from two of it upwards, so that the rounding never
// turns 47 hours into "2 days" or "1 day".
func roughly(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return strconv.FormatInt(n, 10) + " " + unit + "s"
	}
	switch {
	case d >= 48*time.Hour:
		return plural(int64((d+12*time.Hour)/(24*time.Hour)), "day")
	case d >= 2*time.Hour:
		return plural(int64((d+30*time.Minute)/time.Hour), "hour")
	case d >= time.Minute:
		return plural(int64(d/time.Minute), "minute")
	default:
		return "less than a minute"
	}
}
