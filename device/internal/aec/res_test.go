package aec

import (
	"math"
	"math/rand"
	"testing"
)

// distortedEcho is what the linear filter cannot model: the far end through
// a clipping loudspeaker (tanh), delayed 33 samples, plus a near-end talker
// (a 300Hz-rich tone burst) in the second half when talk is true.
func distortedEcho(far []int16, talk bool) (mic []int16, near []int16) {
	mic = make([]int16, len(far))
	near = make([]int16, len(far))
	for i := range far {
		if i >= 33 {
			x := float64(far[i-33]) / 32768
			mic[i] = int16(12000 * math.Tanh(3*x))
		}
		if talk && i > len(far)/2 {
			t := float64(i) / sampleRate
			near[i] = int16(3000 * (math.Sin(2*math.Pi*300*t) + 0.5*math.Sin(2*math.Pi*900*t)))
			mic[i] = int16(math.Max(-32768, math.Min(32767, float64(mic[i])+float64(near[i]))))
		}
	}
	return mic, near
}

// noise is syllabic, like speech or a vocal line: bursts of ~150ms at a few
// per second. Steady noise is the wrong test — speexdsp's noise tracker
// learns a stationary residual as background, and at a 0dB noise floor then
// leaves it alone, which is right for a fan and wrong for an echo.
func noise(n int, seed int64) []int16 {
	r := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	for i := range out {
		env := 0.5 + 0.5*math.Sin(2*math.Pi*3.3*float64(i)/sampleRate)
		out[i] = int16(r.NormFloat64() * 9000 * env * env)
	}
	return out
}

// residualDb runs mic/far through c and returns the output level over the
// last quarter (converged), in dB relative to the mic.
func run(c *Canceller, mic, far []int16) []int16 {
	out := make([]int16, 0, len(mic))
	for f := 0; (f+1)*FrameSize <= len(mic); f++ {
		lo, hi := f*FrameSize, (f+1)*FrameSize
		res := c.ProcessWithRef(toBytes(mic[lo:hi]), toBytes(far[lo:hi]))
		for i := 0; i < FrameSize; i++ {
			out = append(out, int16(uint16(res[2*i])|uint16(res[2*i+1])<<8))
		}
	}
	return out
}

func level(s []int16) float64 {
	var e float64
	for _, v := range s {
		e += float64(v) * float64(v)
	}
	return 10 * math.Log10(e/float64(len(s))+1e-9)
}

func TestResidualSuppressionRemovesDistortedEcho(t *testing.T) {
	far := noise(sampleRate*8, 1)
	mic, _ := distortedEcho(far, false)
	tail := func(s []int16) []int16 { return s[len(s)*3/4:] }

	lin := hwCanceller(64)
	linOut := run(lin, mic, far)
	res := hwCanceller(64)
	res.SetResidual(true, -40, -15)
	resOut := run(res, mic, far)

	linDb := level(tail(mic)) - level(tail(linOut))
	resDb := level(tail(mic)) - level(tail(resOut))
	t.Logf("distorted echo removed: linear %.1fdB, with residual suppression %.1fdB", linDb, resDb)
	if resDb < linDb+6 {
		t.Fatalf("residual suppression added %.1fdB over the linear filter, want >= 6", resDb-linDb)
	}
}

func TestResidualSuppressionOffIsTheLinearFilter(t *testing.T) {
	far := noise(sampleRate*2, 2)
	mic, _ := distortedEcho(far, false)
	a, b := hwCanceller(64), hwCanceller(64)
	b.SetResidual(true, -40, -15)
	b.SetResidual(false, -40, -15)
	oa, ob := run(a, mic, far), run(b, mic, far)
	for i := range oa {
		if oa[i] != ob[i] {
			t.Fatalf("sample %d differs with the suppressor switched off: %d vs %d", i, oa[i], ob[i])
		}
	}
}

// The near-end talker must survive: the suppressor's known cost is cutting
// the person talking over the playback, which is the wake word.
func TestResidualSuppressionKeepsTheNearEnd(t *testing.T) {
	far := noise(sampleRate*8, 3)
	mic, near := distortedEcho(far, true)
	c := hwCanceller(64)
	c.SetResidual(true, -40, -15)
	out := run(c, mic, far)
	seg := func(s []int16) []int16 { return s[len(s)*3/4:] }
	// Correlate the output with the near-end signal over the talk segment:
	// how much of the talker comes through, in dB.
	o, n := seg(out), seg(near[:len(out)])
	var on, nn float64
	for i := range o {
		on += float64(o[i]) * float64(n[i])
		nn += float64(n[i]) * float64(n[i])
	}
	kept := 20 * math.Log10(math.Abs(on/nn)+1e-9)
	t.Logf("near-end kept through double talk: %.1fdB", kept)
	if kept < -10 {
		t.Fatalf("near-end talker attenuated %.1fdB, want at most 10", -kept)
	}
}
