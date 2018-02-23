package ffmpeg

import (
	"errors"
	"fmt"
	"github.com/golang/glog"
	"strconv"
	"strings"
	"unsafe"
)

// #cgo pkg-config: libavformat libavfilter
// #include <stdlib.h>
// #include "lpms_ffmpeg.h"
import "C"

var ErrTranscoderRes = errors.New("TranscoderInvalidResolution")

func RTMPToHLS(localRTMPUrl string, outM3U8 string, tmpl string, seglen_secs string) error {
	inp := C.CString(localRTMPUrl)
	outp := C.CString(outM3U8)
	ts_tmpl := C.CString(tmpl)
	seglen := C.CString(seglen_secs)
	ret := int(C.lpms_rtmp2hls(inp, outp, ts_tmpl, seglen))
	C.free(unsafe.Pointer(inp))
	C.free(unsafe.Pointer(outp))
	C.free(unsafe.Pointer(ts_tmpl))
	C.free(unsafe.Pointer(seglen))
	if 0 != ret {
		glog.Infof("RTMP2HLS Transmux Return : %v\n", Strerror(ret))
		return ErrorMap[ret]
	}
	return nil
}

func Transcode(input string, ps []VideoProfile) error {
	inp := C.CString(input)
	params := make([]C.output_params, len(ps))
	for i, param := range ps {
		oname := C.CString(fmt.Sprintf("out%v%v", i, input))
		res := strings.Split(param.Resolution, "x")
		if len(res) < 2 {
			return ErrTranscoderRes
		}
		w, err := strconv.Atoi(res[0])
		if err != nil {
			return err
		}
		h, err := strconv.Atoi(res[1])
		if err != nil {
			return err
		}
		br := strings.Replace(param.Bitrate, "k", "000", 1)
		bitrate, err := strconv.Atoi(br)
		if err != nil {
			return err
		}
		fps := C.AVRational{num: C.int(param.Framerate), den: 1}
		params[i] = C.output_params{fname: oname, fps: fps,
			w: C.int(w), h: C.int(h), bitrate: C.int(bitrate)}
	}
	ret := int(C.lpms_transcode(inp, (*C.output_params)(&params[0]), C.int(len(params))))
	C.free(unsafe.Pointer(inp))
	if 0 != ret {
		glog.Infof("Transcoder Return : %v\n", Strerror(ret))
		return ErrorMap[ret]
	}
	return nil
}

func InitFFmpeg() {
	C.lpms_init()
}

func DeinitFFmpeg() {
	C.lpms_deinit()
}
