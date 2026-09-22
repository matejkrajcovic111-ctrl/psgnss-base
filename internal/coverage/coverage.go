// Package coverage estimates how far from this base station its corrections
// stay usable, from what the receiver is actually broadcasting.
//
// Two separate things end a baseline:
//
//   - Accuracy. Double-differenced residuals grow with distance once the
//     atmosphere over the rover stops matching the atmosphere over the base.
//     That is the familiar "few mm + n ppm" specification.
//   - Ambiguity resolution. Further out, the ionospheric residual exceeds a
//     fraction of the carrier wavelength and a fix either stops happening or
//     stops being trustworthy. On a dual-frequency stream this is normally the
//     binding limit, and it is the one an operator notices.
//
// Both depend on what this base sends -- how many constellations, on how many
// bands -- so the figure is computed from the live stream rather than
// configured. It is a planning figure for the coverage circle on the map, not
// a guarantee: local ionospheric activity moves it in both directions.
package coverage

import (
	"fmt"
	"math"
)

// Capability is what the base is observed to be broadcasting right now.
type Capability struct {
	// Constellations carrying observations, SBAS excluded: SBAS is a
	// correction service, not a ranging constellation for RTK.
	Constellations int
	// Bands is the number of distinct carrier frequencies observed. Two or
	// more let the rover form an ionosphere-free combination.
	Bands int
}

// Range is the answer, with both limits kept so the UI can say which one
// binds rather than presenting one number with no provenance.
type Range struct {
	RangeKM        int    `json:"range_km"`
	AccuracyKM     int    `json:"accuracy_km"`
	AmbiguityKM    int    `json:"ambiguity_km"`
	Limit          string `json:"limit"` // "ambiguity", "accuracy" or "" when unknown
	Constellations int    `json:"constellations"`
	Bands          int    `json:"bands"`
	Note           string `json:"note"`
}

const (
	// Horizontal repeatability of a fixed solution on a zero baseline.
	fixNoiseMM = 8.0
	// The horizontal accuracy an RTK user still calls usable.
	budgetMM = 50.0
	// Distance dependence of the residual error. A single-frequency rover
	// cannot model the ionosphere at all, so its error grows several times
	// faster.
	multiBandPPM  = 1.0
	singleBandPPM = 5.0
	// Where a reliable fix stops. Single-frequency RTK is short-baseline work
	// whatever else is in the stream; with two or more bands each further
	// constellation adds redundancy that holds the fix out a little longer.
	singleBandARKM       = 12.0
	arBaseKM             = 20.0
	arPerConstellationKM = 7.0
	arCeilingKM          = 60.0
)

// Estimate computes the usable range. A capability with nothing in it returns
// a zero estimate: no range is drawn rather than a guessed one.
func Estimate(c Capability) Range {
	if c.Constellations <= 0 || c.Bands <= 0 {
		return Range{Note: "No observations are being broadcast, so no usable range can be computed."}
	}
	ppm, arKM := multiBandPPM, arBaseKM+arPerConstellationKM*float64(c.Constellations-1)
	if c.Bands < 2 {
		ppm, arKM = singleBandPPM, singleBandARKM
	}
	arKM = math.Min(arKM, arCeilingKM)
	accKM := math.Sqrt(budgetMM*budgetMM-fixNoiseMM*fixNoiseMM) / ppm

	e := Range{AccuracyKM: int(math.Round(accKM)), AmbiguityKM: int(math.Round(arKM)),
		Constellations: c.Constellations, Bands: c.Bands}
	e.RangeKM, e.Limit = e.AmbiguityKM, "ambiguity"
	if e.AccuracyKM < e.AmbiguityKM {
		e.RangeKM, e.Limit = e.AccuracyKM, "accuracy"
	}
	reason := "a reliable ambiguity fix"
	if e.Limit == "accuracy" {
		reason = fmt.Sprintf("%.0f mm horizontal accuracy", budgetMM)
	}
	e.Note = fmt.Sprintf("%d constellations on %d bands: %s holds to about %d km "+
		"(ambiguity %d km, accuracy budget %d km). Planning figure; ionospheric activity moves it either way.",
		c.Constellations, c.Bands, reason, e.RangeKM, e.AmbiguityKM, e.AccuracyKM)
	return e
}
