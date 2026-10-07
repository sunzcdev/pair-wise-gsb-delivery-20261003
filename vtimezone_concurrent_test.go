package ics

import (
	"sync"
	"testing"
	"time"
)

func TestResolveConcurrent(t *testing.T) {
	def := parseOneTimezone(t, nyLike)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			year := 2000 + i
			_, _ = def.Resolve(time.Date(year, 6, 1, 12, 0, 0, 0, time.UTC))
			_, _ = def.Resolve(time.Date(year, 1, 1, 12, 0, 0, 0, time.UTC))
		}(i)
	}
	wg.Wait()
}

func TestResolveUntil(t *testing.T) {
	// Spring switches only in 2020 and 2021 (UNTIL). Offset persists after.
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/Until\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200301T000000\r\nRRULE:FREQ=YEARLY;UNTIL=20210301T000000;BYMONTH=3;BYMONTHDAY=1\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	r, err := def.Resolve(time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || r.OffsetSeconds != 3600 {
		t.Fatalf("until window off=%d err=%v", r.OffsetSeconds, err)
	}
	r, err = def.Resolve(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || r.OffsetSeconds != 3600 {
		t.Fatalf("post-until off=%d err=%v", r.OffsetSeconds, err)
	}
}
