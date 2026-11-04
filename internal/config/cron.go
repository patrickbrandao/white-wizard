package config

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed six-field cron expression with seconds
// ("second minute hour day-of-month month day-of-week"), evaluated in a
// time zone. Each field accepts "*" (or "?"), "*/n", "a", "a-b", "a-b/n",
// "a/n" and comma-separated lists of those. Day of week is 0–7 (0 and 7 are
// Sunday). As in classic cron, when both day of month and day of week are
// restricted a day matches if either matches.
type Cron struct {
	sec, min, hour, dom, month, dow uint64
	domStar, dowStar                bool
	loc                             *time.Location
}

type cronField struct {
	name     string
	min, max int
}

var cronFields = [6]cronField{
	{"second", 0, 59},
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7},
}

// ParseCron parses expr; loc nil means UTC.
func ParseCron(expr string, loc *time.Location) (*Cron, error) {
	parts := strings.Fields(expr)
	if len(parts) != 6 {
		return nil, fmt.Errorf("cron %q: want 6 fields (second minute hour day-of-month month day-of-week), got %d", expr, len(parts))
	}
	if loc == nil {
		loc = time.UTC
	}
	c := &Cron{loc: loc}
	dst := [6]*uint64{&c.sec, &c.min, &c.hour, &c.dom, &c.month, &c.dow}
	for i, p := range parts {
		m, star, err := parseCronField(p, cronFields[i])
		if err != nil {
			return nil, fmt.Errorf("cron %q: %w", expr, err)
		}
		*dst[i] = m
		switch i {
		case 3:
			c.domStar = star
		case 5:
			c.dowStar = star
		}
	}
	if c.dow&(1<<7) != 0 {
		c.dow = c.dow&^(1<<7) | 1 // 7 is Sunday
	}
	return c, nil
}

// parseCronField returns the bit set of field f described by s and whether s
// is an unrestricted "*" (or "?").
func parseCronField(s string, f cronField) (uint64, bool, error) {
	var mask uint64
	for _, item := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(item, "/")
		lo, hi := f.min, f.max
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, false, fmt.Errorf("%s: invalid step %q", f.name, stepStr)
			}
			step = n
		}
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = cronValue(a, f); err != nil {
				return 0, false, err
			}
			if hi, err = cronValue(b, f); err != nil {
				return 0, false, err
			}
			if lo > hi {
				return 0, false, fmt.Errorf("%s: range %q is backwards", f.name, rng)
			}
		default:
			v, err := cronValue(rng, f)
			if err != nil {
				return 0, false, err
			}
			lo = v
			if !hasStep {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, s == "*" || s == "?", nil
}

func cronValue(s string, f cronField) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid value %q", f.name, s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%s: value %d out of range %d-%d", f.name, v, f.min, f.max)
	}
	return v, nil
}

// prevBit returns the highest set bit of mask that is <= x, or -1.
func prevBit(mask uint64, x int) int {
	if x < 0 {
		return -1
	}
	return bits.Len64(mask&(uint64(2)<<uint(x)-1)) - 1
}

func (c *Cron) dayMatches(m time.Month, d int, wd time.Weekday) bool {
	if c.month&(1<<uint(m)) == 0 {
		return false
	}
	domOK, dowOK := c.dom&(1<<uint(d)) != 0, c.dow&(1<<uint(wd)) != 0
	if c.domStar || c.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// Prev returns the latest firing time f with earliest <= f <= t, if any.
// The search walks back day by day only as far as earliest, so its cost is
// bounded by the length of [earliest, t] in days, not in seconds.
func (c *Cron) Prev(t, earliest time.Time) (time.Time, bool) {
	lt := t.In(c.loc)
	y, mo, d := lt.Date()
	h, mi, s := lt.Clock()
	for i := 0; ; i++ {
		day := time.Date(y, mo, d-i, 0, 0, 0, 0, c.loc)
		dy, dm, dd := day.Date()
		if i > 0 && time.Date(dy, dm, dd, 23, 59, 59, 0, c.loc).Before(earliest) {
			return time.Time{}, false
		}
		if !c.dayMatches(dm, dd, day.Weekday()) {
			continue
		}
		maxH := 23
		if i == 0 {
			maxH = h
		}
		for hh := prevBit(c.hour, maxH); hh >= 0; hh = prevBit(c.hour, hh-1) {
			maxM := 59
			if i == 0 && hh == h {
				maxM = mi
			}
			for mm := prevBit(c.min, maxM); mm >= 0; mm = prevBit(c.min, mm-1) {
				maxS := 59
				if i == 0 && hh == h && mm == mi {
					maxS = s
				}
				ss := prevBit(c.sec, maxS)
				if ss < 0 {
					continue
				}
				f := time.Date(dy, dm, dd, hh, mm, ss, 0, c.loc)
				if f.After(t) || f.Hour() != hh || f.Minute() != mm {
					continue // the wall time does not exist (DST gap): no firing
				}
				if f.Before(earliest) {
					return time.Time{}, false
				}
				return f, true
			}
		}
	}
}
