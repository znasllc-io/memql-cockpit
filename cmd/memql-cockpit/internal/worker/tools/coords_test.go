package tools

import (
	"strings"
	"testing"
)

// Pure coordinate-model tests (memql-cockpit#163). No display, no
// build tag: these run headlessly in CI and pin the contract that
// model coordinates live in the emitted screenshot's image space.

func TestFitWithin(t *testing.T) {
	cases := []struct {
		name         string
		w, h, max    int
		wantW, wantH int
	}{
		{name: "no upscale below limit", w: 800, h: 600, max: 1568, wantW: 800, wantH: 600},
		{name: "exactly at limit unchanged", w: 1568, h: 980, max: 1568, wantW: 1568, wantH: 980},
		{name: "retina 2x landscape", w: 3456, h: 2234, max: 1568, wantW: 1568, wantH: 1014},
		{name: "retina 2x portrait", w: 2234, h: 3456, max: 1568, wantW: 1014, wantH: 1568},
		{name: "1.25 scale capture", w: 1920, h: 1080, max: 1568, wantW: 1568, wantH: 882},
		{name: "square", w: 4000, h: 4000, max: 1000, wantW: 1000, wantH: 1000},
		{name: "extreme aspect floors short edge at 1", w: 10000, h: 2, max: 1568, wantW: 1568, wantH: 1},
		{name: "degenerate zero width returned as-is", w: 0, h: 500, max: 1568, wantW: 0, wantH: 500},
		{name: "degenerate max returned as-is", w: 640, h: 480, max: 0, wantW: 640, wantH: 480},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotW, gotH := FitWithin(tc.w, tc.h, tc.max)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Fatalf("FitWithin(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tc.w, tc.h, tc.max, gotW, gotH, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestCoordinateMapperToLogical(t *testing.T) {
	// Geometries used across the table.
	retina := NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014)  // 2x Retina + downscale
	scale150 := NewCoordinateMapper(1280, 800, 1920, 1200, 1568, 980)  // 1.5x HiDPI + downscale
	scale125 := NewCoordinateMapper(1536, 864, 1920, 1080, 1568, 882)  // 1.25x fractional + downscale
	identity := NewCoordinateMapper(1024, 768, 1024, 768, 1024, 768)   // 1x, no downscale
	retinaRaw := NewCoordinateMapper(700, 500, 1400, 1000, 1400, 1000) // 2x, capture small enough to skip downscale

	cases := []struct {
		name         string
		m            CoordinateMapper
		x, y         int
		wantX, wantY int
	}{
		{name: "retina origin", m: retina, x: 0, y: 0, wantX: 0, wantY: 0},
		{name: "retina center-ish", m: retina, x: 784, y: 507, wantX: 864, wantY: 559},
		{name: "retina far corner stays on screen", m: retina, x: 1567, y: 1013, wantX: 1727, wantY: 1116},
		{name: "1.5x mapped point", m: scale150, x: 784, y: 490, wantX: 640, wantY: 400},
		{name: "1.5x far corner", m: scale150, x: 1567, y: 979, wantX: 1279, wantY: 799},
		{name: "1.25x mapped point", m: scale125, x: 1567, y: 881, wantX: 1535, wantY: 863},
		{name: "identity passes through", m: identity, x: 512, y: 384, wantX: 512, wantY: 384},
		{name: "identity far corner", m: identity, x: 1023, y: 767, wantX: 1023, wantY: 767},
		// 2x with no downscale: edge pixel maps to logicalW-0.5 which
		// rounds half away from zero to logicalW; the clamp must pull
		// it back inside the logical rect.
		{name: "2x raw edge rounds then clamps", m: retinaRaw, x: 1399, y: 999, wantX: 699, wantY: 499},
		{name: "2x raw even pixel", m: retinaRaw, x: 700, y: 500, wantX: 350, wantY: 250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotX, gotY := tc.m.ToLogical(tc.x, tc.y)
			if gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("ToLogical(%d,%d) = (%d,%d), want (%d,%d)",
					tc.x, tc.y, gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}
}

func TestCoordinateMapperToEmitted(t *testing.T) {
	retina := NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014)
	cases := []struct {
		name         string
		x, y         int
		wantX, wantY int
	}{
		{name: "origin", x: 0, y: 0, wantX: 0, wantY: 0},
		{name: "logical corner maps inside emitted", x: 1727, y: 1116, wantX: 1567, wantY: 1013},
		{name: "mid point", x: 864, y: 559, wantX: 784, wantY: 507},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotX, gotY := retina.ToEmitted(tc.x, tc.y)
			if gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("ToEmitted(%d,%d) = (%d,%d), want (%d,%d)",
					tc.x, tc.y, gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}
}

// TestCoordinateMapperRoundTrip pins that emitted -> logical ->
// emitted lands within one pixel of where it started across the
// supported scale factors, so repeated mapping cannot drift.
func TestCoordinateMapperRoundTrip(t *testing.T) {
	mappers := map[string]CoordinateMapper{
		"retina2x": NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014),
		"hidpi1.5": NewCoordinateMapper(1280, 800, 1920, 1200, 1568, 980),
		"frac1.25": NewCoordinateMapper(1536, 864, 1920, 1080, 1568, 882),
		"identity": NewCoordinateMapper(1024, 768, 1024, 768, 1024, 768),
	}
	for name, m := range mappers {
		t.Run(name, func(t *testing.T) {
			points := [][2]int{
				{0, 0},
				{1, 1},
				{m.EmittedW / 2, m.EmittedH / 2},
				{m.EmittedW - 1, m.EmittedH - 1},
				{m.EmittedW / 3, (m.EmittedH * 2) / 3},
			}
			for _, p := range points {
				lx, ly := m.ToLogical(p[0], p[1])
				if lx < 0 || ly < 0 || lx >= m.LogicalW || ly >= m.LogicalH {
					t.Fatalf("ToLogical(%d,%d) = (%d,%d) escaped logical rect %dx%d",
						p[0], p[1], lx, ly, m.LogicalW, m.LogicalH)
				}
				ex, ey := m.ToEmitted(lx, ly)
				if abs(ex-p[0]) > 1 || abs(ey-p[1]) > 1 {
					t.Fatalf("round trip (%d,%d) -> (%d,%d) -> (%d,%d) drifted more than 1px",
						p[0], p[1], lx, ly, ex, ey)
				}
			}
		})
	}
}

func TestValidatePointInRect(t *testing.T) {
	cases := []struct {
		name       string
		x, y, w, h int
		wantOK     bool
	}{
		{name: "origin inside", x: 0, y: 0, w: 1568, h: 1014, wantOK: true},
		{name: "far corner inside", x: 1567, y: 1013, w: 1568, h: 1014, wantOK: true},
		{name: "x at width is outside", x: 1568, y: 0, w: 1568, h: 1014, wantOK: false},
		{name: "y at height is outside", x: 0, y: 1014, w: 1568, h: 1014, wantOK: false},
		{name: "negative x", x: -1, y: 10, w: 1568, h: 1014, wantOK: false},
		{name: "negative y", x: 10, y: -1, w: 1568, h: 1014, wantOK: false},
		{name: "empty rect always rejects", x: 0, y: 0, w: 0, h: 0, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail := validatePointInRect("mouse_move", tc.x, tc.y, tc.w, tc.h)
			if tc.wantOK {
				if fail != nil {
					t.Fatalf("expected (%d,%d) inside %dx%d, got failure: %s",
						tc.x, tc.y, tc.w, tc.h, fail.GetErrorMessage())
				}
				return
			}
			if fail == nil {
				t.Fatalf("expected (%d,%d) outside %dx%d to fail, got nil", tc.x, tc.y, tc.w, tc.h)
			}
			if fail.GetErrorCode() != "out_of_bounds" {
				t.Fatalf("error code = %q, want out_of_bounds", fail.GetErrorCode())
			}
			if tc.w > 0 && tc.h > 0 && !strings.Contains(fail.GetErrorMessage(), "(0,0)..(1567,1013)") {
				t.Fatalf("message should name the valid rect, got: %s", fail.GetErrorMessage())
			}
			if !strings.Contains(fail.GetErrorMessage(), "mouse_move") {
				t.Fatalf("message should carry the action label, got: %s", fail.GetErrorMessage())
			}
		})
	}
}

func TestClampLongEdge(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{in: 100, want: 512},
		{in: 512, want: 512},
		{in: 1568, want: 1568},
		{in: 8000, want: 8000},
		{in: 99999, want: 8000},
	}
	for _, tc := range cases {
		if got := clampLongEdge(tc.in); got != tc.want {
			t.Fatalf("clampLongEdge(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
