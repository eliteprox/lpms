package ffmpeg

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"testing"
)

func setupTest(t *testing.T) (func(cmd string), string) {
	dir, err := ioutil.TempDir("", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	InitFFmpeg() // hide some log noise

	// Executes the given bash script and checks the results.
	// The script is passed two arguments:
	// a tempdir and the current working directory.
	cmdFunc := func(cmd string) {
		out, err := exec.Command("bash", "-c", cmd, dir, wd).CombinedOutput()
		if err != nil {
			t.Error(string(out[:]))
		}
	}
	return cmdFunc, dir
}

func TestSegmenter_DeleteSegments(t *testing.T) {
	// Ensure that old segments are deleted as they fall off the playlist

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// sanity check that segmented outputs > playlist length
	cmd := `
		set -eux
		cd "$0"
		# default test.ts is a bit short so make it a bit longer
		cp "$1/../transcoder/test.ts" test.ts
		ffmpeg -loglevel warning -i "concat:test.ts|test.ts|test.ts" -c copy long.ts
		ffmpeg -loglevel warning -i long.ts -c copy -f hls -hls_time 1 long.m3u8
		# ensure we have more segments than playlist length
		[ $(ls long*.ts | wc -l) -ge 6 ]
	`
	run(cmd)

	// actually do the segmentation
	err := RTMPToHLS(dir+"/long.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// check that segments have been deleted by counting output ts files
	cmd = `
		set -eux
		cd "$0"
		[ $(ls out_*.ts | wc -l) -eq 6 ]
	`
	run(cmd)
}

func TestSegmenter_StreamOrdering(t *testing.T) {
	// Ensure segmented output contains [video, audio] streams in that order
	// regardless of stream ordering in the input

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input that has a subtitle, audio and video stream, in that order
	cmd := `
	    set -eux
	    cd "$0"

		# generate subtitle file
		cat <<- EOF > inp.srt
			1
			00:00:00,000 --> 00:00:01,000
			hi
		EOF

		# borrow the test.ts from the transcoder dir, output with 3 streams
		ffmpeg -loglevel warning -i inp.srt -i "$1/../transcoder/test.ts" -c:a copy -c:v copy -c:s mov_text -t 1 -map 0:s -map 1:a -map 1:v test.mp4

		# some sanity checks. these will exit early on a nonzero code
		# check stream count, then indexes of subtitle, audio and video
		[ $(ffprobe -loglevel warning -i test.mp4 -show_streams | grep index | wc -l) -eq 3 ]
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams s | grep index=0
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams a | grep index=1
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams v | grep index=2
	`
	run(cmd)

	// actually do the segmentation
	err := RTMPToHLS(dir+"/test.mp4", dir+"/out.m3u8", dir+"/out_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// check stream ordering in output file. Should be video, then audio
	cmd = `
		set -eux
		cd $0
		[ $(ffprobe -loglevel warning -i out_0.ts -show_streams | grep index | wc -l) -eq 2 ]
		ffprobe -loglevel warning -i out_0.ts -show_streams -select_streams v | grep index=0
		ffprobe -loglevel warning -i out_0.ts -show_streams -select_streams a | grep index=1
	`
	run(cmd)
}

func TestSegmenter_DropLatePackets(t *testing.T) {
	// Certain sources sometimes send packets with out-of-order FLV timestamps
	// (eg, ManyCam on Android when the phone can't keep up)
	// Ensure we drop these packets

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input with an out-of-order timestamp
	cmd := `
		set -eux
		cd "$0"
		# borrow segmenter test file, rewrite a timestamp
		cp "$1/../segmenter/test.flv" test.flv

		# Sanity check the last few timestamps are monotonic : 18867,18900,18933
		ffprobe -loglevel quiet -show_packets -select_streams v test.flv | grep dts= | tail -3 | tr '\n' ',' | grep dts=18867,dts=18900,dts=18933,

		# replace ts 18900 at position 2052736 with ts 18833 (0x4991 hex)
		printf '\x49\x91' | dd of=test.flv bs=1 seek=2052736 count=2 conv=notrunc
		# sanity check timestamps are now 18867,18833,18933
		ffprobe -loglevel quiet -show_packets -select_streams v test.flv | grep dts= | tail -3 | tr '\n' ',' | grep dts=18867,dts=18833,dts=18933,

		# sanity check number of frames
		ffprobe -loglevel quiet -count_packets -show_streams -select_streams v test.flv | grep nb_read_packets=569
	`
	run(cmd)

	err := RTMPToHLS(dir+"/test.flv", dir+"/out.m3u8", dir+"/out_%d.ts", "100", 0)
	if err != nil {
		t.Error(err)
	}

	// Now ensure things are as expected
	cmd = `
		set -eux
		cd "$0"

		# check monotonic timestamps (rescaled for the 90khz mpegts timebase)
		ffprobe -loglevel quiet -show_packets -select_streams v out_0.ts | grep dts= | tail -3 | tr '\n' ',' | grep dts=1694970,dts=1698030,dts=1703970,

		# check that we dropped the packet
		ffprobe -loglevel quiet -count_packets -show_streams -select_streams v out_0.ts | grep nb_read_packets=568
	`
	run(cmd)
}

func TestTranscoder_UnevenRes(t *testing.T) {
	// Ensure transcoding still works on input with uneven resolutions
	// and that aspect ratio is maintained

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input with an uneven res
	cmd := `
	    set -eux
	    cd "$0"

		# borrow the test.ts from the transcoder dir, output with 123x456 res
		ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:a copy -c:v mpeg4 -s 123x456 test.mp4

		# sanity check resulting resolutions
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams v | grep width=123
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams v | grep height=456

		# and generate another sample with an odd value in the larger dimension
		ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:a copy -c:v mpeg4 -s 123x457 test_larger.mp4
		ffprobe -loglevel warning -i test_larger.mp4 -show_streams -select_streams v | grep width=123
		ffprobe -loglevel warning -i test_larger.mp4 -show_streams -select_streams v | grep height=457

	`
	run(cmd)

	err := Transcode(dir+"/test.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	err = Transcode(dir+"/test_larger.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Check output resolutions
	cmd = `
		set -eux
		cd "$0"
		ffprobe -loglevel warning -show_streams -select_streams v out0test.mp4 | grep width=64
		ffprobe -loglevel warning -show_streams -select_streams v out0test.mp4 | grep height=240
		ffprobe -loglevel warning -show_streams -select_streams v out0test_larger.mp4 | grep width=64
		ffprobe -loglevel warning -show_streams -select_streams v out0test_larger.mp4 | grep height=240
	`
	run(cmd)

	// Transpose input and do the same checks as above.
	cmd = `
		set -eux
		cd "$0"
		ffmpeg -loglevel warning -i test.mp4 -c:a copy -c:v mpeg4 -vf transpose transposed.mp4

		# sanity check resolutions
		ffprobe -loglevel warning -show_streams -select_streams v transposed.mp4 | grep width=456
		ffprobe -loglevel warning -show_streams -select_streams v transposed.mp4 | grep height=123
	`
	run(cmd)

	err = Transcode(dir+"/transposed.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Check output resolutions for transposed input
	cmd = `
		set -eux
		cd "$0"
		ffprobe -loglevel warning -show_streams -select_streams v out0transposed.mp4 | grep width=426
		ffprobe -loglevel warning -show_streams -select_streams v out0transposed.mp4 | grep height=114
	`
	run(cmd)

	// check special case of square resolutions
	cmd = `
		set -eux
		cd "$0"
		ffmpeg -loglevel warning -i test.mp4 -c:a copy -c:v mpeg4 -s 123x123 square.mp4

		# sanity check resolutions
		ffprobe -loglevel warning -show_streams -select_streams v square.mp4 | grep width=123
		ffprobe -loglevel warning -show_streams -select_streams v square.mp4 | grep height=123
	`
	run(cmd)

	err = Transcode(dir+"/square.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Check output resolutions are still square
	cmd = `
		set -eux
		cd "$0"
		ls
		ffprobe -loglevel warning -i out0square.mp4 -show_streams -select_streams v | grep width=426
		ffprobe -loglevel warning -i out0square.mp4 -show_streams -select_streams v | grep height=426
	`
	run(cmd)

	// TODO set / check sar/dar values?
}

func TestTranscoder_SampleRate(t *testing.T) {

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input with 48khz audio
	cmd := `
		set -eux
		cd $0

		# borrow the test.ts from the transcoder dir, output with 48khz audio
		ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:v copy -af 'aformat=sample_fmts=fltp:channel_layouts=stereo:sample_rates=48000' -c:a aac -t 1.1 test.ts

		# sanity check results to ensure preconditions
		ffprobe -loglevel warning -show_streams -select_streams a test.ts | grep sample_rate=48000

		# output timestamp check as a script to reuse for post-transcoding check
		cat <<- 'EOF' > check_ts
			set -eux
			# ensure 1 second of timestamps add up to within 2.1% of 90khz (mpegts timebase)
			# 2.1% is the margin of error, 1024 / 48000 (% increase per frame)
			# 1024 = samples per frame, 48000 = samples per second

			# select last frame pts, subtract from first frame pts, check diff
			ffprobe -loglevel warning -show_frames  -select_streams a "$2"  | grep pkt_pts= | head -"$1" | awk 'BEGIN{FS="="} ; NR==1 { fst = $2 } ; END{ diff=(($2-fst)/90000); exit diff <= 0.979 || diff >= 1.021 }'
		EOF
		chmod +x check_ts

		# check timestamps at the given frame offsets. 47 = ceil(48000/1024)
		./check_ts 47 test.ts

		# check failing cases; use +2 since we may be +/- the margin of error
		[ $(./check_ts 45 test.ts || echo "shouldfail") = "shouldfail" ]
		[ $(./check_ts 49 test.ts || echo "shouldfail") = "shouldfail" ]
	`
	run(cmd)

	err := Transcode(dir+"/test.ts", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Ensure transcoded sample rate is 44k.1hz and check timestamps
	cmd = `
		set -eux
		cd "$0"
		ffprobe -loglevel warning -show_streams -select_streams a out0test.ts | grep sample_rate=44100

		# Sample rate = 44.1khz, samples per frame = 1024
		# Frames per second = ceil(44100/1024) = 44

		# Technically check_ts margin of error is 2.1% due to 48khz rate
		# At 44.1khz, error is 2.3% so we'll just accept the tighter bounds

		# check timestamps at the given frame offsets. 44 = ceil(48000/1024)
		./check_ts 44 out0test.ts

		# check failing cases; use +2 since we may be +/- the margin of error
		[ $(./check_ts 46 out0test.ts || echo "shouldfail") = "shouldfail" ]
		[ $(./check_ts 42 out0test.ts || echo "shouldfail") = "shouldfail" ]
	`
	run(cmd)

}

func TestTranscoder_Timestamp(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
		set -eux
		cd $0

		# prepare the input and sanity check 60fps
		cp "$1/../transcoder/test.ts" inp.ts
		ffprobe -loglevel warning -select_streams v -show_streams -count_frames inp.ts > inp.out
		grep avg_frame_rate=60 inp.out
		grep r_frame_rate=60 inp.out

		# reduce 60fps original to 30fps indicated but 15fps real
		ffmpeg -loglevel warning -i inp.ts -t 1 -c:v libx264 -an -vf select='not(mod(n\,4))' -r 30 test.ts
		ffprobe -loglevel warning -select_streams v -show_streams -count_frames test.ts > test.out

		# sanity check some properties. hard code numbers for now.
		grep avg_frame_rate=30 test.out
		grep r_frame_rate=15 test.out
		grep nb_read_frames=15 test.out
		grep duration_ts=90000 test.out
		grep start_pts=138000 test.out
	`
	run(cmd)

	err := Transcode(dir+"/test.ts", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	cmd = `
		set -eux
		cd $0

		# hardcode some checks for now. TODO make relative to source.
		ffprobe -loglevel warning -select_streams v -show_streams -count_frames out0test.ts > test.out
		grep avg_frame_rate=30 test.out
		grep r_frame_rate=30 test.out
		grep nb_read_frames=28 test.out
		grep duration_ts=84000 test.out
		grep start_pts=138000 test.out
	`
	run(cmd)
}

func TestTranscoderStatistics_Decoded(t *testing.T) {
	// Checks the decoded stats returned after transcoding

	var (
		totalPixels int64
		totalFrames int
	)

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// segment using our muxer. This should produce 4 segments.
	err := RTMPToHLS("../transcoder/test.ts", dir+"/test.m3u8", dir+"/test_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// Use various resolutions to test input
	// Quickcheck style tests would be nice here one day?
	profiles := []VideoProfile{P144p30fps16x9, P240p30fps16x9, P360p30fps16x9, P576p30fps16x9}

	// Transcode some data, save encoded statistics, then attempt to re-transcode
	// Ensure decoded re-transcode stats match original transcoded statistics
	for i, p := range profiles {
		oname := fmt.Sprintf("%s/out_%d.ts", dir, i)
		out := []TranscodeOptions{TranscodeOptions{Profile: p, Oname: oname}}
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/test_%d.ts", dir, i)}
		res, err := Transcode3(in, out)
		if err != nil {
			t.Error(err)
		}
		info := res.Encoded[0]

		// Now attempt to re-encode the transcoded data
		// Pass in an empty output to achieve a decode-only flow
		// and check decoded results from *that*
		in = &TranscodeOptionsIn{Fname: oname}
		res, err = Transcode3(in, nil)
		if err != nil {
			t.Error(err)
		}
		w, h, err := VideoProfileResolution(p)
		if err != nil {
			t.Error(err)
		}

		// Check pixel counts
		if info.Pixels != res.Decoded.Pixels {
			t.Error("Mismatched pixel counts")
		}
		if info.Pixels != int64(w*h*res.Decoded.Frames) {
			t.Error("Mismatched pixel counts")
		}
		// Check frame counts
		if info.Frames != res.Decoded.Frames {
			t.Error("Mismatched frame counts")
		}
		if info.Frames != int(res.Decoded.Pixels/int64(w*h)) {
			t.Error("Mismatched frame counts")
		}
		totalPixels += info.Pixels
		totalFrames += info.Frames
	}

	// Now for something fun. Concatenate our segments of various resolutions
	// Run them through the transcoder, and check the sum of pixels / frames match
	// Ensures we can properly accommodate mid-stream resolution changes.
	cmd := `
        set -eux
        cd "$0"
        cat out_0.ts out_1.ts out_2.ts out_3.ts > combined.ts
    `
	run(cmd)
	in := &TranscodeOptionsIn{Fname: dir + "/combined.ts"}
	res, err := Transcode3(in, nil)
	if err != nil {
		t.Error(err)
	}
	if totalPixels != res.Decoded.Pixels {
		t.Error("Mismatched total pixel counts")
	}
	if totalFrames != res.Decoded.Frames {
		t.Errorf("Mismatched total frame counts - %d vs %d", totalFrames, res.Decoded.Frames)
	}
}

func TestTranscoder_Statistics_Encoded(t *testing.T) {
	// Checks the encoded stats returned after transcoding

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        set -eux
        cd $0

        # prepare 1-second input
        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -loglevel warning -i inp.ts -c:a copy -c:v copy -t 1 test.ts
    `
	run(cmd)

	// set a 60fps input at a small resolution (to help runtime)
	p144p60fps := P144p30fps16x9
	p144p60fps.Framerate = 60
	// odd / nonstandard input just to sanity check.
	podd123fps := VideoProfile{Resolution: "124x70", Framerate: 123, Bitrate: "100k"}

	// Construct output parameters.
	// Quickcheck style tests would be nice here one day?
	profiles := []VideoProfile{P240p30fps16x9, P144p30fps16x9, p144p60fps, podd123fps}
	out := make([]TranscodeOptions, len(profiles))
	for i, p := range profiles {
		out[i] = TranscodeOptions{Profile: p, Oname: fmt.Sprintf("%s/out%d.mp4", dir, i)}
	}

	res, err := Transcode3(&TranscodeOptionsIn{Fname: dir + "/test.ts"}, out)
	if err != nil {
		t.Error(err)
	}

	for i, r := range res.Encoded {
		w, h, err := VideoProfileResolution(out[i].Profile)
		if err != nil {
			t.Error(err)
		}

		// Check pixel counts
		if r.Pixels != int64(w*h*r.Frames) {
			t.Error("Mismatched pixel counts")
		}
		// Since this is a 1-second input we should ideally have count of frames
		if r.Frames != int(out[i].Profile.Framerate) {
			t.Error("Mismatched frame counts")
		}

		// Check frame counts against ffprobe-reported output

		// First, generate stats file
		f, err := os.Create(fmt.Sprintf("%s/out%d.res.stats", dir, i))
		if err != nil {
			t.Error(err)
		}
		b := bufio.NewWriter(f)
		fmt.Fprintf(b, `[STREAM]
width=%d
height=%d
nb_read_frames=%d
[/STREAM]
`, w, h, r.Frames)
		b.Flush()
		f.Close()

		cmd = fmt.Sprintf(`
            set -eux
            cd $0

            fname=out%d

            ffprobe -loglevel warning -hide_banner -count_frames  -select_streams v -show_entries stream=width,height,nb_read_frames $fname.mp4 > $fname.stats
            ls -lha
            diff -u $fname.stats $fname.res.stats
		`, i)

		run(cmd)
	}
}

func TestTranscoder_StatisticsAspectRatio(t *testing.T) {
	// Check that we correctly account for aspect ratio adjustments
	//  Eg, the transcoded resolution we receive may be smaller than
	//  what we initially requested

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        set -eux
        cd $0

        # prepare 1-second input
        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -loglevel warning -i inp.ts -c:a copy -c:v copy -t 1 test.ts
    `
	run(cmd)

	// This will be adjusted to 124x70 by the rescaler (since source is 16:9)
	pAdj := VideoProfile{Resolution: "124x456", Framerate: 15, Bitrate: "100k"}
	out := []TranscodeOptions{TranscodeOptions{Profile: pAdj, Oname: dir + "/adj.mp4"}}
	res, err := Transcode3(&TranscodeOptionsIn{Fname: dir + "/test.ts"}, out)
	if err != nil || len(res.Encoded) <= 0 {
		t.Error(err)
	}
	r := res.Encoded[0]
	if r.Frames != int(pAdj.Framerate) || r.Pixels != int64(r.Frames*124*70) {
		t.Error(fmt.Errorf("Results did not match: %v ", r))
	}
}

func TestMuxerOpts(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Prepare test environment : truncate input file
	cmd := `
        set -eux
        cd $0

        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -i inp.ts -c:a copy -c:v copy -t 1 inp-short.ts
    `
	run(cmd)

	prof := P240p30fps16x9

	// Set the muxer itself given a different extension
	_, err := Transcode3(&TranscodeOptionsIn{
		Fname: dir + "/inp-short.ts",
	}, []TranscodeOptions{TranscodeOptions{
		Oname:   dir + "/out-mkv.mp4",
		Profile: prof,
		Muxer:   ComponentOpts{Name: "matroska"},
	}})

	if err != nil {
		t.Error(err)
	}

	// Pass in some options to muxer
	_, err = Transcode3(&TranscodeOptionsIn{
		Fname: dir + "/inp.ts",
	}, []TranscodeOptions{TranscodeOptions{
		Oname:   dir + "/out.mpd",
		Profile: prof,
		Muxer: ComponentOpts{
			Name: "dash",
			Opts: map[string]string{
				"media_seg_name": "lpms-test-$RepresentationID$-$Number%05d$.m4s",
				"init_seg_name":  "lpms-init-$RepresentationID$.m4s",
			},
		},
	}})
	if err != nil {
		t.Error(err)
	}

	cmd = `
        set -eux
        cd $0

        # check formats and that options were used
        ffprobe -loglevel warning -show_format out-mkv.mp4 | grep format_name=matroska
        # ffprobe -loglevel warning -show_format out.mpd | grep format_name=dash # this fails so skip for now

        # concat headers. mp4 chunks are annoying
        cat lpms-init-0.m4s lpms-test-0-00001.m4s > video.m4s
        cat lpms-init-1.m4s lpms-test-1-00001.m4s > audio.m4s
        ffprobe -show_format video.m4s | grep nb_streams=1
        ffprobe -show_format audio.m4s | grep nb_streams=1
        ffprobe -show_streams -select_streams v video.m4s | grep codec_name=h264
        ffprobe -show_streams -select_streams a audio.m4s | grep codec_name=aac
    `
	run(cmd)
}

func TestEncoderOpts(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Prepare test environment : truncate input file
	cmd := `
        set -eux
        cd $0

        # truncate input
        ffmpeg -i "$1/../transcoder/test.ts" -c:a copy -c:v copy -t 1 test.ts

        # we will sanity check image quality with ssim
        # since ssim needs res and framecount to match, sanity check those
        ffprobe -show_streams -select_streams v test.ts | grep width=1280
        ffprobe -show_streams -select_streams v test.ts | grep height=720
        ffprobe -count_frames -show_streams -select_streams v test.ts | grep nb_read_frames=60
    `
	run(cmd)

	prof := P720p60fps16x9
	in := &TranscodeOptionsIn{Fname: dir + "/test.ts"}
	out := []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/out.nut",
		Profile:      prof,
		VideoEncoder: ComponentOpts{Name: "snow"},
		AudioEncoder: ComponentOpts{
			Name: "vorbis",
			// required since vorbis implementation is marked experimental
			// also, gives us an opportunity to test the audio opts
			Opts: map[string]string{"strict": "experimental"}},
	}}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}

	cmd = `
        set -eux
        cd $0

        # Check codecs are what we expect them to be
        ffprobe -show_streams -select_streams v out.nut | grep codec_name=snow
        ffprobe -show_streams -select_streams a out.nut | grep codec_name=vorbis

        # sanity check image quality : compare using ssim
        ffmpeg -loglevel warning -i out.nut -i test.ts -lavfi '[0:v][1:v]ssim=stats.log' -f null -
        # ensure that no more than 5 frames have ssim < 0.95
        grep -Po 'All:\K\d+.\d+' stats.log | awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'
    `
	run(cmd)
}

func TestStreamCopyAndDrop(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	out := []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/videoonly.mp4",
		VideoEncoder: ComponentOpts{Name: "copy"},
		AudioEncoder: ComponentOpts{Name: "drop"},
	}, TranscodeOptions{
		Oname:        dir + "/audioonly.mp4",
		VideoEncoder: ComponentOpts{Name: "drop"},
		AudioEncoder: ComponentOpts{Name: "copy"},
	}, TranscodeOptions{
		// Avoids ADTS to ASC conversion
		// which changes the bitstream
		Oname:        dir + "/audioonly.ts",
		VideoEncoder: ComponentOpts{Name: "drop"},
		AudioEncoder: ComponentOpts{Name: "copy"},
	}, TranscodeOptions{
		Oname:        dir + "/audio.md5",
		VideoEncoder: ComponentOpts{Name: "drop"},
		AudioEncoder: ComponentOpts{Name: "copy"},
		Muxer:        ComponentOpts{Name: "md5"},
	}, TranscodeOptions{
		Oname:        dir + "/video.md5",
		VideoEncoder: ComponentOpts{Name: "copy"},
		AudioEncoder: ComponentOpts{Name: "drop"},
		Muxer:        ComponentOpts{Name: "md5"},
	}, TranscodeOptions{
		Oname:        dir + "/copy.mp4",
		VideoEncoder: ComponentOpts{Name: "copy"},
		AudioEncoder: ComponentOpts{Name: "copy"},
	}}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd := `
        set -eux
        cd $0

        cp "$1"/../transcoder/test.ts .

        # truncate input for later use
        ffmpeg -i test.ts -t 1 -c:a copy -c:v copy test-short.ts

        # check some results
        ffprobe -loglevel warning -show_format videoonly.mp4 | grep nb_streams=1
        ffprobe -loglevel warning -show_streams videoonly.mp4 | grep codec_name=h264

        ffprobe -loglevel warning -show_format audioonly.mp4 | grep nb_streams=1
        ffprobe -loglevel warning -show_streams audioonly.mp4 | grep codec_name=aac

        ffprobe -loglevel warning -show_format copy.mp4 | grep nb_streams=2

        # Verify video md5sum
        ffmpeg -i test.ts -an -c:v copy -f md5 ffmpeg-video-orig.md5
        diff -u video.md5 ffmpeg-video-orig.md5

        # Verify audio md5sums
        ffmpeg -i test.ts -vn -c:a copy -f md5 ffmpeg-audio-orig.md5
        ffmpeg -i audioonly.ts -vn -c:a copy -f md5 ffmpeg-audio-ts.md5
        ffmpeg -i copy.mp4 -vn -c:a copy -f md5 ffmpeg-audio-copy.md5
        ffmpeg -i audioonly.mp4 -c:a copy -f md5 ffmpeg-audio-mp4.md5
        diff -u audio.md5 ffmpeg-audio-orig.md5
        diff -u audio.md5 ffmpeg-audio-ts.md5
        diff -u ffmpeg-audio-mp4.md5 ffmpeg-audio-copy.md5

        # TODO test timestamps? should they be copied?
    `
	run(cmd)

	// Test specifying a copy or a drop for a stream that does not exist
	in.Fname = dir + "/videoonly.mp4"
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/videoonly-copy.mp4",
		VideoEncoder: ComponentOpts{Name: "copy"},
		AudioEncoder: ComponentOpts{Name: "copy"},
	}, TranscodeOptions{
		Oname:        dir + "/videoonly-copy-2.mp4",
		VideoEncoder: ComponentOpts{Name: "copy"},
		AudioEncoder: ComponentOpts{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err, in.Fname)
	}

	// Test mp4-to-mpegts; involves an implicit bitstream conversion to annex B
	in.Fname = dir + "/videoonly.mp4"
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/videoonly-copy.ts",
		VideoEncoder: ComponentOpts{Name: "copy"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	// sanity check the md5sum of the mp4-to-mpegts result
	in.Fname = dir + "/videoonly-copy.ts"
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/videoonly-copy.md5",
		VideoEncoder: ComponentOpts{Name: "copy"},
		Muxer:        ComponentOpts{Name: "md5"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd = `
        set -eux
        cd "$0"

        # use ffmpeg to convert the existing mp4 to ts and check match
        # for some reason this does NOT match the original mpegts
        ffmpeg -i videoonly.mp4 -c:v copy -f mpegts ffmpeg-mp4to.ts
        ffmpeg -i ffmpeg-mp4to.ts -c:v copy -f md5 ffmpeg-mp4tots.md5
        diff -u videoonly-copy.md5 ffmpeg-mp4tots.md5
    `
	run(cmd)

	// Test failure of audio copy in mpegts-to-(not-mp4). eg, flv
	// Fixing this requires using the aac_adtstoasc bitstream filter.
	// (mp4 muxer automatically inserts it if necessary; others don't)
	in.Fname = "../transcoder/test.ts"
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        "fail.flv",
		VideoEncoder: ComponentOpts{Name: "drop"},
		AudioEncoder: ComponentOpts{Name: "copy"},
	}}
	_, err = Transcode3(in, out)
	if err == nil || err.Error() != "Invalid data found when processing input" {
		t.Error("Expected error converting audio from ts to flv but got ", err)
	}

	// Test error when trying to mux no streams
	in.Fname = "../transcoder/test.ts"
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/none.mp4",
		VideoEncoder: ComponentOpts{Name: "drop"},
		AudioEncoder: ComponentOpts{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err == nil || err.Error() != "Invalid argument" {
		t.Error("Did not get expected error: ", err)
	}

	// Test error when missing profile in default video configuration
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/profile.mp4",
		AudioEncoder: ComponentOpts{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err == nil || err != ErrTranscoderRes {
		t.Error("Expected res err related to profile, but got ", err)
	}

	// Encode one stream of a short sample while copying / dropping another
	in.Fname = dir + "/test-short.ts"
	out = []TranscodeOptions{TranscodeOptions{
		Oname:        dir + "/encoded-video.mp4",
		Profile:      P144p30fps16x9,
		AudioEncoder: ComponentOpts{Name: "drop"},
	}, TranscodeOptions{
		Oname:        dir + "/encoded-audio.mp4",
		VideoEncoder: ComponentOpts{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)

	}

	// Sanity check default transcode options with single-stream input
	in.Fname = dir + "/encoded-video.mp4"
	out = []TranscodeOptions{TranscodeOptions{Oname: dir + "/encoded-video2.mp4", Profile: P144p30fps16x9}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	in.Fname = dir + "/encoded-audio.mp4"
	out = []TranscodeOptions{TranscodeOptions{Oname: dir + "/encoded-audio2.mp4", Profile: P144p30fps16x9}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
}
