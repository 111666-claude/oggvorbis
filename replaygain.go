package oggvorbis

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ReplayGain tag names as stored in the vorbis comment header.
const (
	TagTrackGain = "REPLAYGAIN_TRACK_GAIN"
	TagTrackPeak = "REPLAYGAIN_TRACK_PEAK"
)

// DefaultPeakLimit is the peak limit used when ReplayGain.PeakLimit is not
// positive.
const DefaultPeakLimit = 1.0

// ReplayGain configures optional ReplayGain processing of the decoded
// samples. The zero value applies the track gain as stored in the file and
// limits peaks to DefaultPeakLimit.
type ReplayGain struct {
	// Preamp is an additional gain in decibels applied on top of the
	// track gain stored in the file. It must be finite.
	Preamp float64
	// PeakLimit is the maximum absolute sample value produced after the
	// gain is applied. Values <= 0 select DefaultPeakLimit. It must be
	// finite.
	PeakLimit float64
}

// SetReplayGain enables ReplayGain processing on the reader: every sample
// returned by Read is scaled by the track gain found in the vorbis comment
// header (plus opts.Preamp) and limited to +/- the configured peak limit.
//
// Tag handling rules:
//   - Tag keys are matched case-insensitively; the value may carry a
//     case-insensitive "dB" suffix (gain) and surrounding whitespace.
//   - If a tag appears multiple times the first occurrence wins; later
//     duplicates are ignored.
//   - A present but malformed or non-finite (NaN/Inf) tag value is an error.
//   - A missing track gain falls back to 0 dB (unity gain).
//   - A missing, zero or negative track peak disables peak-based scaling;
//     samples are still clamped to the peak limit.
//
// With peak protection the effective gain is reduced so that
// gain*peak does not exceed the peak limit. Output samples are always
// clamped to +/- the peak limit and NaN samples are mapped to 0, so the
// reader never emits NaN or out-of-range values while enabled.
//
// SetReplayGain only changes sample values; the number of samples returned
// by Read and the values reported by Position and Length are unaffected.
func (r *Reader) SetReplayGain(opts ReplayGain) error {
	if math.IsNaN(opts.Preamp) || math.IsInf(opts.Preamp, 0) {
		return errors.New("oggvorbis: replaygain preamp must be finite")
	}
	if math.IsNaN(opts.PeakLimit) || math.IsInf(opts.PeakLimit, 0) {
		return errors.New("oggvorbis: replaygain peak limit must be finite")
	}
	limit := opts.PeakLimit
	if limit <= 0 {
		limit = DefaultPeakLimit
	}
	gainDB, peak, hasPeak, err := scanReplayGainTags(r.dec.CommentHeader.Comments)
	if err != nil {
		return err
	}
	gain := math.Pow(10, (gainDB+opts.Preamp)/20)
	if hasPeak && gain*peak > limit {
		gain = limit / peak
	}
	r.gain = float32(gain)
	r.peakLimit = float32(limit)
	r.replayGain = true
	return nil
}

// scanReplayGainTags extracts the track gain (in dB) and track peak from
// vorbis comments. See SetReplayGain for the parsing rules.
func scanReplayGainTags(comments []string) (gain, peak float64, hasPeak bool, err error) {
	var seenGain, seenPeak bool
	for _, c := range comments {
		eq := strings.IndexByte(c, '=')
		if eq < 0 {
			continue
		}
		key, value := c[:eq], strings.TrimSpace(c[eq+1:])
		switch {
		case strings.EqualFold(key, TagTrackGain):
			if seenGain {
				continue
			}
			seenGain = true
			gain, err = parseGainValue(value)
			if err != nil {
				return 0, 0, false, fmt.Errorf("oggvorbis: invalid %s value %q: %w", TagTrackGain, value, err)
			}
		case strings.EqualFold(key, TagTrackPeak):
			if seenPeak {
				continue
			}
			seenPeak = true
			peak, err = parsePeakValue(value)
			if err != nil {
				return 0, 0, false, fmt.Errorf("oggvorbis: invalid %s value %q: %w", TagTrackPeak, value, err)
			}
		}
	}
	// A non-positive peak carries no information; treat it as absent.
	return gain, peak, seenPeak && peak > 0, nil
}

func parseGainValue(s string) (float64, error) {
	if len(s) >= 2 && strings.EqualFold(s[len(s)-2:], "dB") {
		s = strings.TrimSpace(s[:len(s)-2])
	}
	return parseFiniteFloat(s)
}

func parsePeakValue(s string) (float64, error) {
	return parseFiniteFloat(s)
}

func parseFiniteFloat(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("value is not finite")
	}
	return v, nil
}
