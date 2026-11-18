package file

import (
	"testing"
	"time"
)

func TestTimeLayoutParse(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		layout string
		text   string
		loc    *time.Location
		want   time.Time
		ok     bool
	}{
		{"european with time", "DD.MM.YYYY hh:mm:ss", "18.11.2026 14:05:09", rome, time.Date(2026, 11, 18, 13, 5, 9, 0, time.UTC), true},
		{"summer offset", "DD.MM.YYYY hh:mm:ss", "18.07.2026 14:05:09", rome, time.Date(2026, 7, 18, 12, 5, 9, 0, time.UTC), true},
		{"date only is midnight", "YYYY-MM-DD", "2026-11-18", time.UTC, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), true},
		{"no separators", "YYYYMMDDhhmmss", "20261118140509", time.UTC, time.Date(2026, 11, 18, 14, 5, 9, 0, time.UTC), true},
		{"surrounding spaces", "YYYY-MM-DD", " 2026-11-18 ", time.UTC, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), true},
		{"impossible day", "DD.MM.YYYY", "31.02.2026", time.UTC, time.Time{}, false},
		{"short year", "DD.MM.YYYY", "18.11.26", time.UTC, time.Time{}, false},
		{"wrong separator", "DD.MM.YYYY", "18/11/2026", time.UTC, time.Time{}, false},
		{"trailing text", "DD.MM.YYYY", "18.11.2026 x", time.UTC, time.Time{}, false},
		{"empty", "DD.MM.YYYY", "", time.UTC, time.Time{}, false},
		{"hour 24", "DD.MM.YYYY hh:mm:ss", "18.11.2026 24:00:00", time.UTC, time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := ParseTimeLayout(tc.layout)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := l.Parse(tc.text, tc.loc)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got.UTC(), tc.want)
			}
		})
	}
}

func TestParseTimeLayoutRefused(t *testing.T) {
	for _, layout := range []string{"", "DD.MM.YY", "YY", "YYYY-MM-DDThh:mm:ss", "DD.MM", "YYYY-MM-DD YYYY", "YYYY-MM-DD 12", "YYYY-MM-DD hh:mm:ss.fff", "hh:mm:ss", "Mon DD.MM.YYYY"} {
		t.Run(layout, func(t *testing.T) {
			if _, err := ParseTimeLayout(layout); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	rome, _ := time.LoadLocation("Europe/Rome")
	layout, err := ParseTimeLayout("DD.MM.YYYY hh:mm:ss")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		spec TimestampSpec
		text string
		want time.Time
		ok   bool
	}{
		{"rfc3339 with offset ignores zone", TimestampSpec{Format: TimestampRFC3339}, "2026-11-18T14:05:09+02:00", time.Date(2026, 11, 18, 12, 5, 9, 0, time.UTC), true},
		{"rfc3339 Z", TimestampSpec{Format: TimestampRFC3339}, "2026-11-18T14:05:09Z", time.Date(2026, 11, 18, 14, 5, 9, 0, time.UTC), true},
		{"rfc3339 fraction", TimestampSpec{Format: TimestampRFC3339}, "2026-11-18T14:05:09.25Z", time.Date(2026, 11, 18, 14, 5, 9, 250e6, time.UTC), true},
		{"rfc3339 without offset uses zone", TimestampSpec{Format: TimestampRFC3339}, "2026-11-18T14:05:09", time.Date(2026, 11, 18, 13, 5, 9, 0, time.UTC), true},
		{"rfc3339 bad", TimestampSpec{Format: TimestampRFC3339}, "yesterday", time.Time{}, false},
		{"unix_s", TimestampSpec{Format: TimestampUnixSeconds}, "1700000000", time.Unix(1700000000, 0).UTC(), true},
		{"unix_ms", TimestampSpec{Format: TimestampUnixMillis}, "1700000000123", time.Unix(1700000000, 123e6).UTC(), true},
		{"unix bad", TimestampSpec{Format: TimestampUnixSeconds}, "abc", time.Time{}, false},
		{"layout", TimestampSpec{Format: "DD.MM.YYYY hh:mm:ss", Layout: layout}, "18.11.2026 14:05:09", time.Date(2026, 11, 18, 13, 5, 9, 0, time.UTC), true},
		{"layout bad", TimestampSpec{Format: "DD.MM.YYYY hh:mm:ss", Layout: layout}, "31.02.2026 14:05:09", time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			got, ok := ParseTimestamp(&spec, tc.text, rome)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got.UTC(), tc.want)
			}
		})
	}
	if _, ok := ParseTimestamp(nil, "x", rome); ok {
		t.Fatal("nil spec")
	}
}
