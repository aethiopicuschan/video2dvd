package video2dvd_test

import (
	. "github.com/aethiopicuschan/video2dvd"
	"testing"
)

func TestDVDMenuButtonNavigationTargetsStayWithinPage(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		start, end               int
		page, pageCount, pgcBase int
		aspect                   AspectRatio
	}{
		{name: "first page with 36 buttons", start: 0, end: 33, page: 0, pageCount: 2, pgcBase: 3, aspect: AspectRatio4x3},
		{name: "short second page", start: 33, end: 35, page: 1, pageCount: 2, pgcBase: 3, aspect: AspectRatio4x3},
		{name: "widescreen first page", start: 0, end: 16, page: 0, pageCount: 1, pgcBase: 2, aspect: AspectRatio16x9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buttons := ExportMakeDVDMenuButtons(tc.start, tc.end, Profile{Aspect: tc.aspect}, tc.page, tc.pageCount, tc.pgcBase)
			if len(buttons) == 0 || len(buttons) > 36 {
				t.Fatalf("button count = %d, want 1..36", len(buttons))
			}
			for index, button := range buttons {
				for direction, target := range map[string]uint8{
					"up": button.Up, "down": button.Down,
					"left": button.Left, "right": button.Right,
				} {
					if target == 0 || int(target) > len(buttons) {
						t.Errorf("button %d %s target = %d, outside 1..%d", index+1, direction, target, len(buttons))
					}
				}
			}
		})
	}
}
