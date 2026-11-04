package plugin

import (
	"math"
	"strings"
	"testing"

	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func wireSample(v *pb.Value) *pb.Sample {
	return &pb.Sample{
		Device:   "press-07",
		Signal:   "state",
		Ts:       timestamppb.Now(),
		Value:    v,
		Quality:  pb.Quality_QUALITY_GOOD,
		Priority: pb.Priority_PRIORITY_BULK,
	}
}

func TestSampleFromWireRefusesInvalidUTF8WithoutTheValue(t *testing.T) {
	const invalid = "RUN\xff\xfe"

	_, err := sampleFromWire(wireSample(&pb.Value{Kind: &pb.Value_StringValue{StringValue: invalid}}))
	if err == nil {
		t.Fatal("invalid UTF-8 string accepted")
	}

	if strings.Contains(err.Error(), "RUN") {
		t.Fatalf("error %q quotes the value", err)
	}
}

func TestSampleFromWireNormalisesNegativeZero(t *testing.T) {
	s, err := sampleFromWire(wireSample(&pb.Value{Kind: &pb.Value_DoubleValue{DoubleValue: math.Copysign(0, -1)}}))
	if err != nil {
		t.Fatalf("sampleFromWire: %v", err)
	}

	if math.Float64bits(s.Value.Float) != 0 {
		t.Fatalf("value bits = %#x, want +0", math.Float64bits(s.Value.Float))
	}
}
