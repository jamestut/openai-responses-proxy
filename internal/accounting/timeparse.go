package accounting

import (
	"regexp"
	"strconv"
	"time"

	// Bundle IANA rules so named zones work when the host has no zoneinfo files.
	_ "time/tzdata"
)

var (
	yearPattern  = regexp.MustCompile(`^[0-9]{4}$`)
	monthPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)
	datePattern  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// parsePeriod returns local calendar boundaries; callers convert them to UTC only
// after AddDate has accounted for daylight-saving transitions.
func parsePeriod(value string, location *time.Location) (time.Time, time.Time, error) {
	switch {
	case yearPattern.MatchString(value):
		year, _ := strconv.Atoi(value)
		if year == 0 {
			return time.Time{}, time.Time{}, queryError("period year must be between 0001 and 9999")
		}
		start := time.Date(year, 1, 1, 0, 0, 0, 0, location)
		// Calendar arithmetic preserves local year boundaries across zone transitions.
		return start, start.AddDate(1, 0, 0), nil
	case monthPattern.MatchString(value):
		year, _ := strconv.Atoi(value[:4])
		month, _ := strconv.Atoi(value[5:])
		if year == 0 || month < 1 || month > 12 {
			return time.Time{}, time.Time{}, queryError("period month is invalid")
		}
		start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, location)
		return start, start.AddDate(0, 1, 0), nil
	case datePattern.MatchString(value):
		date, err := time.ParseInLocation(time.DateOnly, value, location)
		// Round-tripping ensures the accepted calendar label matches the requested date.
		if err != nil || date.Format(time.DateOnly) != value || date.Year() == 0 {
			return time.Time{}, time.Time{}, queryError("period date is invalid")
		}
		return date, date.AddDate(0, 0, 1), nil
	default:
		return time.Time{}, time.Time{}, queryError("period must be YYYY, YYYY-MM, or YYYY-MM-DD")
	}
}

// parseBoundary interprets date-only values in the selected zone and requires an
// explicit offset for date-time values, which already identify an instant.
func parseBoundary(value string, location *time.Location) (time.Time, error) {
	if datePattern.MatchString(value) {
		date, err := time.ParseInLocation(time.DateOnly, value, location)
		if err != nil || date.Format(time.DateOnly) != value || date.Year() == 0 {
			return time.Time{}, queryError("date boundary is invalid")
		}
		return date, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Year() == 0 {
		return time.Time{}, queryError("timed boundaries must be RFC3339 with an explicit offset")
	}
	return parsed, nil
}
