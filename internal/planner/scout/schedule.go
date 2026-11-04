package scout

import "time"

// slots are a weekday's scheduled runs, local time: every two hours from
// 08:00 to 20:00, and 08:15, before the 08:30 morning brief (§15.6 频率).
// Constants, not config.
var slots = [][2]int{{8, 0}, {8, 15}, {10, 0}, {12, 0}, {14, 0}, {16, 0}, {18, 0}, {20, 0}}

// due reports the slot a run is due for at now, given the last run's
// start: the latest of today's slots at or before now, when last started
// before it. A machine that slept through slots runs once when it wakes,
// for the latest one only; a day's missed slots are not carried into the
// next day, and weekends have none.
func due(now, last time.Time) (time.Time, bool) {
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return time.Time{}, false
	}
	var slot time.Time
	for _, hm := range slots {
		t := time.Date(now.Year(), now.Month(), now.Day(), hm[0], hm[1], 0, 0, now.Location())
		if t.After(now) {
			break
		}
		slot = t
	}
	if slot.IsZero() || !last.Before(slot) {
		return time.Time{}, false
	}
	return slot, true
}
