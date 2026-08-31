// Package capture orchestrates one satellite pass: SatDump invocation in a
// per-pass work directory, live output parsing, frame/SNR stats, audio
// retention, and the pass state machine. Replaces RN2's receive_noaa.sh and
// receive_meteor.sh.
package capture

import (
	"fmt"
	"strconv"

	"github.com/perhp/rnv3/internal/config"
)

// BuildArgs constructs the satdump argument list for one pass. Pure function
// so the per-receiver/per-satellite flag matrix is unit-testable.
//
// Flag parity notes vs RN2:
//   - The work dir is explicit (RN2 relied on the at-job's cwd via a
//     deliberately-undefined variable).
//   - PPM correction is RTL-SDR-only (RN2 blanked it for everything else).
//   - Gain uses each source's real SatDump parameter (RN2 sent Airspy's
//     --general_gain to every non-RTL device, where it was ignored); devices
//     without a scalar gain get none, and extra_satdump_args carries
//     device-specific knobs.
//   - Gain is passed verbatim, 0 included (RN2 behavior).
func BuildArgs(cfg *config.Config, sat config.Satellite, workDir string, captureSeconds int, startTS int64) ([]string, error) {
	recv, ok := config.ReceiverTypes[cfg.SDR.Type]
	if !ok {
		return nil, fmt.Errorf("unknown receiver type %q", cfg.SDR.Type)
	}

	var pipeline string
	switch sat.Type {
	case config.SatNOAAAPT:
		pipeline = "noaa_apt"
	case config.SatMeteorLRPT:
		pipeline = "meteor_m2-x_lrpt"
		if sat.Interleaving80k {
			pipeline = "meteor_m2-x_lrpt_80k"
		}
	default:
		return nil, fmt.Errorf("satellite %s: unsupported type %q", sat.Name, sat.Type)
	}

	args := []string{
		"live", pipeline, workDir,
		"--source", recv.Source,
		"--samplerate", recv.SampleRate,
		"--frequency", strconv.FormatFloat(sat.FrequencyMHz, 'f', -1, 64) + "e6",
	}
	if recv.SupportsPPM {
		args = append(args, "--ppm_correction", strconv.FormatFloat(sat.FreqOffset, 'f', -1, 64))
	}
	if cfg.SDR.UseDeviceString && sat.DeviceID != "" {
		args = append(args, "--source_id", sat.DeviceID)
	}
	if recv.GainFlag != "" {
		args = append(args, recv.GainFlag, strconv.FormatFloat(sat.Gain, 'f', -1, 64))
	}
	if sat.BiasTee {
		args = append(args, "--bias")
	}
	args = append(args, sat.ExtraSatdumpArgs...)

	switch sat.Type {
	case config.SatNOAAAPT:
		args = append(args,
			"--satellite_number", strconv.Itoa(sat.SatelliteNumber),
			"--sdrpp_noise_reduction",
			"--start_timestamp", strconv.FormatInt(startTS, 10),
			"--save_wav",
		)
		if cfg.Processing.NOAA.CropWedges {
			args = append(args, "--autocrop_wedges")
		}
	case config.SatMeteorLRPT:
		args = append(args, "--fill_missing")
	}

	args = append(args,
		"--finish_processing",
		"--timeout", strconv.Itoa(captureSeconds),
	)
	return args, nil
}
