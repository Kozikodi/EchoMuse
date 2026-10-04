package als

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The threshold policy decides what reaches Home Assistant promptly and what
// waits up to 30s for the stats tick. Too sensitive and a flickering lamp
// floods the control plane; too coarse and "someone turned a light on" is the
// thing it misses.
func TestSignificant(t *testing.T) {
	cases := []struct {
		name          string
		baseline, now int
		want          bool
	}{
		// Measured noise on a still room: 309/311/313/308/312 on consecutive
		// reads, about ±1.5%. None of it may report.
		{"still room drift up", 309, 313, false},
		{"still room drift down", 313, 308, false},

		// The case this exists for.
		{"lamp switched on", 40, 300, true},
		{"lamp switched off", 300, 40, true},

		// Hand over the sensor, measured 309 -> 0.
		{"covered", 309, 0, true},
		{"uncovered", 0, 308, true},

		// Near darkness must not produce infinite ratios: a 2 lux wobble in a
		// dark room is not a room lighting up.
		{"tiny change near zero", 0, 2, false},
		{"tiny change near zero, down", 3, 0, false},

		// Big RELATIVE change but small absolute — still noise-ish.
		{"5 to 9 lux", 5, 9, false},

		// Daylight: 50 lux is invisible against 20000 and must not report,
		// which an absolute threshold would get wrong.
		{"daylight jitter", 20000, 20050, false},
		{"cloud clears", 8000, 20000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Significant(c.baseline, c.now); got != c.want {
				t.Fatalf("Significant(%d, %d) = %v, want %v",
					c.baseline, c.now, got, c.want)
			}
		})
	}
}

// Symmetry matters: a light going off should be as reportable as one coming
// on. Comparing against the baseline rather than the new value is what makes
// that true, and it is easy to get backwards.
func TestSignificantIsSymmetricEnough(t *testing.T) {
	if !Significant(300, 40) || !Significant(40, 300) {
		t.Fatal("a lamp must be reportable in both directions")
	}
}

// ── Status reporting (issue #90) ─────────────────────────────────────────────
//
// Two users' Dots reported no ambient light sensor and there was no way to
// tell whether the chip was absent or the driver had not bound, because the
// answer was only ever written to a log file on the device. These pin the
// three verdicts against a fake i2c bus.

// fakeBus builds an i2c tree and points the package at it. Returns the root.
// Also resets the package's cached state, which otherwise leaks between tests
// and makes the second one assert against the first one's scan.
func fakeBus(t *testing.T, devices map[string]bool) {
	t.Helper()
	root := t.TempDir()
	for name, withAttr := range devices {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if withAttr {
			if err := os.WriteFile(filepath.Join(dir, "als_lux"), []byte("42\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	mu.Lock()
	path, scale, lastScan, reported = "", 1, time.Time{}, false
	status = Status{Code: StatusUnknown}
	mu.Unlock()

	// IIO and idme default to empty fixtures, so no test reads the host's.
	empty := t.TempDir()
	oldI2C, oldIIO, oldCal := i2cGlob, iioGlob, alscalPaths
	i2cGlob = filepath.Join(root, "*", "name")
	iioGlob = filepath.Join(empty, "iio:device*", "name")
	alscalPaths = []string{filepath.Join(empty, "alscal")}
	t.Cleanup(func() {
		i2cGlob, iioGlob, alscalPaths = oldI2C, oldIIO, oldCal
		mu.Lock()
		path, scale, lastScan, reported = "", 1, time.Time{}, false
		status = Status{Code: StatusUnknown}
		mu.Unlock()
	})
}

// fakeIIO adds an IIO device with the given name and illuminance0_input
// reading, plus an idme alscal (omitted when empty).
func fakeIIO(t *testing.T, name string, ill int, alscal string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "iio:device0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, v string) {
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "name"), name+"\n")
	write(filepath.Join(dir, iioAttr), strconv.Itoa(ill)+"\n")
	// Present on the real device and fatal to read. A fixture value that
	// would be WRONG makes any accidental read show up in the lux.
	write(filepath.Join(dir, "calibrated_lux"), "99999\n")
	iioGlob = filepath.Join(root, "iio:device*", "name")
	if alscal != "" {
		cal := filepath.Join(root, "alscal")
		write(cal, alscal)
		alscalPaths = []string{cal}
	}
}

// The real calibration string off a G090LF107 (2026-10-04), with the NUL
// procfs and the device tree end it with.
const realAlscal = " ams_0_0=0,0.000,0 ams_400_0=266,306.000,36\x00"

func TestStatusOK(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2540": true, "tsl2584tsv": false})
	got := Report()
	if got.Code != StatusOK {
		t.Fatalf("code = %q, want %q (detail %q)", got.Code, StatusOK, got.Detail)
	}
	if got.Path == "" {
		t.Fatal("a resolved sensor must report where it was found")
	}
	if !Present() {
		t.Fatal("Present() must be true when the sensor resolves")
	}
}

// Seen must list the WHOLE bus even when the sensor is found early.
//
// The first version returned as soon as it matched, so a working device
// reported only the names sorting before tsl2540 — on real hardware that
// dropped is31fl3236, tlv320aic32x4 and bq24297. Comparing a healthy bus
// against a broken one is what this field is for, so a truncated list from
// the healthy side defeats the purpose. The fixtures missed it because none
// had a device sorting after the match; `zz_after` is here to guarantee one
// always does.
func TestSeenListsWholeBusWhenSensorFound(t *testing.T) {
	fakeBus(t, map[string]bool{
		"aa_before": false,
		"tsl2540":   true,
		"zz_after":  false,
	})
	got := Report()
	if got.Code != StatusOK {
		t.Fatalf("code = %q, want %q", got.Code, StatusOK)
	}
	if len(got.Seen) != 3 {
		t.Fatalf("Seen = %v, want all three bus devices", got.Seen)
	}
	var sawAfter bool
	for _, s := range got.Seen {
		if s == "zz_after" {
			sawAfter = true
		}
	}
	if !sawAfter {
		t.Fatalf("Seen = %v, missing the device that sorts after the sensor", got.Seen)
	}
}

// The hypothesis for #90: these units carry the second ALS, which is present
// on working devices too but has no usable driver, and not the tsl2540.
func TestStatusNoChip(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2584tsv": false, "tlv320aic3101": false})
	got := Report()
	if got.Code != StatusNoChip {
		t.Fatalf("code = %q, want %q", got.Code, StatusNoChip)
	}
	// Seen is what makes the answer verifiable rather than merely asserted,
	// and identifies an unfamiliar revision the first time one appears.
	if len(got.Seen) != 2 {
		t.Fatalf("Seen = %v, want both bus devices", got.Seen)
	}
	if Present() {
		t.Fatal("Present() must be false with no tsl2540")
	}
}

// Distinct from no_chip because the fixes are opposite: this one is ours.
func TestStatusNoAttribute(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2540": false})
	got := Report()
	if got.Code != StatusNoAttribute {
		t.Fatalf("code = %q, want %q", got.Code, StatusNoAttribute)
	}
	if got.Detail == "" {
		t.Fatal("a failure must carry a detail a human can read in a bundle")
	}
}

// The absence LOG is deliberately once-only; the status must not be, or a
// device whose bus changed would keep reporting its first answer forever.
func TestStatusRefreshesAcrossScans(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2584tsv": false})
	if got := Report(); got.Code != StatusNoChip {
		t.Fatalf("first scan: code = %q, want %q", got.Code, StatusNoChip)
	}
	mu.Lock()
	lastScan = time.Time{} // allow an immediate re-scan
	mu.Unlock()
	if got := Report(); got.Code != StatusNoChip {
		t.Fatalf("second scan: code = %q, want %q", got.Code, StatusNoChip)
	}
}

// ── The second-sourced ALS (#90) ─────────────────────────────────────────────

// The G090LF096/LF107 batch: tsl2540 listed on i2c with no als_lux, and the
// tsl2584tsv answering on IIO. Measured on hardware: illuminance0_input 25
// with coeff 306 is 400*25/306 = 32.7 lux.
func TestIIOFallbackCalibrated(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2540": false, "tsl2584tsv": false})
	fakeIIO(t, "tsl2584tsv", 25, realAlscal)

	got := Report()
	if got.Code != StatusOK {
		t.Fatalf("code = %q, want %q (detail %q)", got.Code, StatusOK, got.Detail)
	}
	if filepath.Base(got.Path) != iioAttr {
		t.Fatalf("path = %q, want the %s attribute", got.Path, iioAttr)
	}
	if !strings.Contains(got.Detail, "306") {
		t.Fatalf("detail %q should name the calibration it used", got.Detail)
	}
	lux := Lux()
	if lux == nil || *lux != 33 {
		t.Fatalf("Lux() = %v, want 33 (25 x 400/306, rounded)", lux)
	}
}

// Without calibration the raw reading still tracks the room; a degraded
// number beats declaring no sensor, and the Detail must say it is degraded.
func TestIIOFallbackUncalibrated(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2540": false})
	fakeIIO(t, "tsl2584tsv", 25, "")

	got := Report()
	if got.Code != StatusOK {
		t.Fatalf("code = %q, want %q", got.Code, StatusOK)
	}
	if !strings.Contains(got.Detail, "uncalibrated") {
		t.Fatalf("detail %q should say the reading is uncalibrated", got.Detail)
	}
	if lux := Lux(); lux == nil || *lux != 25 {
		t.Fatalf("Lux() = %v, want the raw 25", lux)
	}
}

// Devices that work today must not change: a readable tsl2540 wins even if
// an IIO light sensor exists too, and keeps scale 1.
func TestTSL2540PreferredOverIIO(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2540": true})
	fakeIIO(t, "tsl2584tsv", 25, realAlscal)

	got := Report()
	if filepath.Base(got.Path) != "als_lux" {
		t.Fatalf("path = %q, want the tsl2540's als_lux", got.Path)
	}
	if lux := Lux(); lux == nil || *lux != 42 {
		t.Fatalf("Lux() = %v, want 42 unscaled", lux)
	}
}

// By name, not by iio:device0: some other IIO device there is not a light
// sensor, and reading its illuminance0_input (if it had one) would be wrong.
func TestIIOMatchedByName(t *testing.T) {
	fakeBus(t, map[string]bool{"tsl2540": false})
	fakeIIO(t, "some-other-sensor", 25, realAlscal)

	if got := Report(); got.Code != StatusNoAttribute {
		t.Fatalf("code = %q, want %q", got.Code, StatusNoAttribute)
	}
	if Present() {
		t.Fatal("an unrelated IIO device must not count as the light sensor")
	}
}

func TestParseAlscal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
		ok   bool
	}{
		{"real, NUL-terminated", realAlscal, 306, true},
		{"no leading space", "ams_0_0=0,0.000,0 ams_400_0=266,306.000,36", 306, true},
		// The driver's sscanf %d truncates; matching it keeps our number
		// equal to what calibrated_lux would have said.
		{"fraction truncated", " ams_400_0=270,207.667,27", 207, true},
		{"trailing newline", " ams_400_0=270,207.667,27\n", 207, true},
		{"no 400 entry", " ams_0_0=0,0.000,0", 0, false},
		{"zero coeff", " ams_400_0=0,0.000,0", 0, false},
		{"one field", " ams_400_0=266", 0, false},
		{"garbage", " ams_400_0=x,y,z", 0, false},
		{"empty", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseAlscal(c.in)
			if got != c.want || ok != c.ok {
				t.Fatalf("parseAlscal(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

// calibrated_lux corrupts the kernel heap on the 2584 batch (package
// comment). No string literal in this package may name it, so no future
// "also read the calibrated value" can reach a device. Comments may.
func TestNeverNamesCalibratedLux(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "als.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
			strings.Contains(lit.Value, "calibrated_lux") {
			t.Errorf("%s: string literal names calibrated_lux, which panics the kernel on the tsl2584 batch",
				fset.Position(lit.Pos()))
		}
		return true
	})
}
