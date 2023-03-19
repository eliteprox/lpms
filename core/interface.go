package core

import (
	"context"

	"github.com/eliteprox/lpms/segmenter"
	"github.com/eliteprox/lpms/stream"
)

//RTMPSegmenter describes an interface for a segmenter
type RTMPSegmenter interface {
	SegmentRTMPToHLS(ctx context.Context, rs stream.RTMPVideoStream, hs stream.HLSVideoStream, segOptions segmenter.SegmenterOptions) error
}
