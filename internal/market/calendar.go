package market

import (
	"fmt"
	"time"
	_ "time/tzdata"
)

const BrusselsTimezone = "Europe/Brussels"

var brusselsLocation = func() *time.Location {
	location, err := time.LoadLocation(BrusselsTimezone)
	if err != nil {
		panic(fmt.Sprintf("load embedded %s timezone: %v", BrusselsTimezone, err))
	}
	return location
}()

func BrusselsDay(value time.Time) string {
	return value.In(brusselsLocation).Format(time.DateOnly)
}

func ParseBrusselsDay(day string) (time.Time, error) {
	return time.ParseInLocation(time.DateOnly, day, brusselsLocation)
}

func NextBrusselsDay(day string) (string, error) {
	value, err := ParseBrusselsDay(day)
	if err != nil {
		return "", err
	}
	return value.AddDate(0, 0, 1).Format(time.DateOnly), nil
}

func BrusselsDayBounds(day string) (time.Time, time.Time, error) {
	start, err := ParseBrusselsDay(day)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start.UTC(), start.AddDate(0, 0, 1).UTC(), nil
}

func LastReliableBrusselsDay(
	incrementalCoveredThrough *time.Time,
	now time.Time,
) (string, bool, error) {
	if incrementalCoveredThrough == nil || incrementalCoveredThrough.IsZero() {
		return "", false, nil
	}
	if incrementalCoveredThrough.After(now) {
		return "", false, nil
	}

	coveredDay, err := ParseBrusselsDay(BrusselsDay(*incrementalCoveredThrough))
	if err != nil {
		return "", false, err
	}
	today, err := ParseBrusselsDay(BrusselsDay(now))
	if err != nil {
		return "", false, err
	}
	if coveredDay.Before(today) {
		coveredDay = coveredDay.AddDate(0, 0, -1)
	}
	return coveredDay.Format(time.DateOnly), true, nil
}
