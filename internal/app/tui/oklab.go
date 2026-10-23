package tui

// OKLCH and OKHSL to sRGB conversion for theme colours. Oklab and OKHSL are
// Björn Ottosson's colour spaces; the OKHSL code follows his reference
// implementation (https://bottosson.github.io/posts/colorpicker/), Copyright
// (c) 2021 Björn Ottosson, used under the MIT license:
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

import (
	"math"

	tauui "github.com/losinggeneration/tau/tui"
)

type vec3 [3]float64

type mat3 [3]vec3

func (m mat3) mul(v vec3) vec3 {
	var out vec3
	for i, row := range m {
		out[i] = row[0]*v[0] + row[1]*v[1] + row[2]*v[2]
	}
	return out
}

var (
	labToLMS = mat3{
		{1, 0.3963377773761749, 0.2158037573099136},
		{1, -0.1055613458156586, -0.0638541728258133},
		{1, -0.0894841775298119, -1.2914855480194092},
	}
	lmsToLinearSRGB = mat3{
		{4.0767416360759583, -3.3077115392580629, 0.2309699031821043},
		{-1.2684379732850315, 2.6097573492876882, -0.341319376002657},
		{-0.0041960761386756, -0.7034186179359362, 1.7076146940746117},
	}
	// Per sRGB channel: the (a, b) half-plane where that channel clips first,
	// and the polynomial approximating the maximum saturation there.
	saturationFit = [3]struct {
		plane  [2]float64
		coeffs [5]float64
	}{
		{[2]float64{-1.8817031, -0.80936501}, [5]float64{1.19086277, 1.76576728, 0.59662641, 0.75515197, 0.56771245}},
		{[2]float64{1.8144408, -1.19445267}, [5]float64{0.73956515, -0.45954404, 0.08285427, 0.12541073, -0.14503204}},
		{[2]float64{0.13110758, 1.81333971}, [5]float64{1.35733652, -0.00915799, -1.1513021, -0.50559606, 0.00692167}},
	}
)

const (
	okhslK1 = 0.206
	okhslK2 = 0.03
	okhslK3 = (1 + okhslK1) / (1 + okhslK2)
)

func oklabToLinearSRGB(lab vec3) vec3 {
	lms := labToLMS.mul(lab)
	for i := range lms {
		lms[i] = lms[i] * lms[i] * lms[i]
	}
	return lmsToLinearSRGB.mul(lms)
}

func linearSRGBToRGB(linear vec3) [3]uint8 {
	var out [3]uint8
	for i, v := range linear {
		if v > 0.0031308 {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		} else {
			v *= 12.92
		}
		out[i] = uint8(math.Round(math.Min(1, math.Max(0, v)) * 255))
	}
	return out
}

func inSRGBGamut(linear vec3) bool {
	const epsilon = 1e-7
	for _, v := range linear {
		if v < -epsilon || v > 1+epsilon {
			return false
		}
	}
	return true
}

// oklchToRGB reduces chroma at a fixed hue until the colour fits sRGB, so an
// out-of-gamut colour keeps its hue and lightness.
func oklchToRGB(l, c, h float64) [3]uint8 {
	sin, cos := math.Sincos(math.Mod(h, 360) * math.Pi / 180)
	at := func(chroma float64) vec3 { return oklabToLinearSRGB(vec3{l, chroma * cos, chroma * sin}) }
	if direct := at(c); inSRGBGamut(direct) {
		return linearSRGBToRGB(direct)
	}
	linear := at(0)
	low, high := 0.0, c
	for range 20 {
		chroma := (low + high) / 2
		if candidate := at(chroma); inSRGBGamut(candidate) {
			low, linear = chroma, candidate
		} else {
			high = chroma
		}
	}
	return linearSRGBToRGB(linear)
}

func lmsSlopes(a, b float64) vec3 {
	var out vec3
	for i, row := range labToLMS {
		out[i] = row[1]*a + row[2]*b
	}
	return out
}

func maxSaturation(a, b float64) float64 {
	channel := 2
	for i := range 2 {
		if p := saturationFit[i].plane; p[0]*a+p[1]*b > 1 {
			channel = i
			break
		}
	}
	k := saturationFit[channel].coeffs
	weights := lmsToLinearSRGB[channel]
	s := k[0] + k[1]*a + k[2]*b + k[3]*a*a + k[4]*a*b
	slopes := lmsSlopes(a, b)
	var f, f1, f2 float64
	for i, slope := range slopes {
		base := 1 + s*slope
		f += weights[i] * base * base * base
		f1 += weights[i] * 3 * slope * base * base
		f2 += weights[i] * 6 * slope * slope * base
	}
	return s - f*f1/(f1*f1-0.5*f*f2)
}

func cusp(a, b float64) (float64, float64) {
	s := maxSaturation(a, b)
	rgb := oklabToLinearSRGB(vec3{1, s * a, s * b})
	l := math.Cbrt(1 / math.Max(rgb[0], math.Max(rgb[1], rgb[2])))
	return l, l * s
}

func maxChroma(a, b, lightness, cuspL, cuspC float64) float64 {
	if lightness <= cuspL {
		return cuspC * lightness / cuspL
	}
	t := cuspC * (lightness - 1) / (cuspL - 1)
	slopes := lmsSlopes(a, b)
	var lms, cubes, first, second vec3
	for i, slope := range slopes {
		lms[i] = lightness + t*slope
		cubes[i] = lms[i] * lms[i] * lms[i]
		first[i] = 3 * slope * lms[i] * lms[i]
		second[i] = 6 * slope * slope * lms[i]
	}
	step := math.MaxFloat64
	for _, row := range lmsToLinearSRGB {
		dot := func(v vec3) float64 { return row[0]*v[0] + row[1]*v[1] + row[2]*v[2] }
		f, f1, f2 := dot(cubes)-1, dot(first), dot(second)
		if u := f1 / (f1*f1 - 0.5*f*f2); u >= 0 {
			step = math.Min(step, -f*u)
		}
	}
	return t + step
}

func chromaStops(l, a, b float64) (c0, cMid, cMax float64) {
	cuspL, cuspC := cusp(a, b)
	cMax = maxChroma(a, b, l, cuspL, cuspC)
	k := cMax / math.Min(l*(cuspC/cuspL), (1-l)*(cuspC/(1-cuspL)))
	midS := 0.11516993 + 1/(7.4477897+4.1590124*b+
		a*(-2.19557347+1.75198401*b+a*(-2.13704948-10.02301043*b+a*(-4.24894561+5.38770819*b+4.69891013*a))))
	midT := 0.11239642 + 1/(1.6132032-0.68124379*b+
		a*(0.40370612+0.90148123*b+a*(-0.27087943+0.6122399*b+a*(0.00299215-0.45399568*b-0.14661872*a))))
	cMid = 0.9 * k * math.Sqrt(math.Sqrt(1/(1/math.Pow(l*midS, 4)+1/math.Pow((1-l)*midT, 4))))
	a0, b0 := l*0.4, (1-l)*0.8
	c0 = math.Sqrt(1 / (1/(a0*a0) + 1/(b0*b0)))
	return c0, cMid, cMax
}

// okhslToRGB converts hue in degrees and saturation and lightness in 0-1.
func okhslToRGB(hue, saturation, lightness float64) [3]uint8 {
	l := (lightness*lightness + okhslK1*lightness) / (okhslK3 * (lightness + okhslK2))
	lab := vec3{l, 0, 0}
	if l > 0 && l < 1 && saturation > 0 {
		b, a := math.Sincos(2 * math.Pi * math.Mod(math.Mod(hue, 360)+360, 360) / 360)
		c0, cMid, cMax := chromaStops(l, a, b)
		var chroma float64
		if saturation < 0.8 {
			t := 1.25 * saturation
			k1 := 0.8 * c0
			chroma = t * k1 / (1 - (1-k1/cMid)*t)
		} else {
			t := 5 * (saturation - 0.8)
			k1 := 0.2 * cMid * cMid * 1.25 * 1.25 / c0
			chroma = cMid + t*k1/(1-(1-k1/(cMax-cMid))*t)
		}
		lab = vec3{l, chroma * a, chroma * b}
	}
	return linearSRGBToRGB(oklabToLinearSRGB(lab))
}

func srgbToLinear(value float64) float64 {
	if value <= 0.04045 {
		return value / 12.92
	}
	return math.Pow((value+0.055)/1.055, 2.4)
}

func rgbToOklab(color tauui.RGBColor) vec3 {
	linear := vec3{srgbToLinear(float64(color.R) / 255), srgbToLinear(float64(color.G) / 255), srgbToLinear(float64(color.B) / 255)}
	lms := (mat3{
		{0.4122214694707629, 0.5363325372617349, 0.0514459932675022},
		{0.2119034958178251, 0.6806995506452344, 0.1073969535369405},
		{0.0883024591900564, 0.2817188391361215, 0.6299787016738222},
	}).mul(linear)
	for i := range lms {
		lms[i] = math.Cbrt(lms[i])
	}
	return (mat3{
		{0.210454268309314, 0.793617774702305, -0.0040720430116193},
		{1.9779985324311684, -2.42859224204858, 0.450593709617411},
		{0.0259040424655478, 0.7827717124575296, -0.8086757549230774},
	}).mul(lms)
}

func oklabToOkhslLightness(l float64) float64 {
	x := okhslK3*l - okhslK1
	return 0.5 * (x + math.Sqrt(x*x+4*okhslK2*okhslK3*l))
}

func rgbToOkhsl(color tauui.RGBColor) (h, s, l float64) {
	lab := rgbToOklab(color)
	chroma := math.Hypot(lab[1], lab[2])
	l = oklabToOkhslLightness(lab[0])
	if chroma < 1e-9 || l <= 0 || l >= 1 {
		return 0, 0, l
	}
	h = math.Mod(math.Atan2(lab[2], lab[1])*180/math.Pi+360, 360)
	c0, cMid, cMax := chromaStops(lab[0], lab[1]/chroma, lab[2]/chroma)
	if chroma < cMid {
		k1 := 0.8 * c0
		s = 0.8 * chroma / (k1 + (1-k1/cMid)*chroma)
	} else {
		k1 := 0.2 * cMid * cMid * 1.25 * 1.25 / c0
		offset := chroma - cMid
		s = 0.8 + 0.2*offset/(k1+(1-k1/(cMax-cMid))*offset)
	}
	return h, min(1, max(0, s)), l
}
