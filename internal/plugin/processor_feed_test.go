package plugin

import (
	"context"
	"errors"
	"testing"

	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
)

// failingProcessStream is a Process stream whose Send fails; the embedded
// interface is nil, so only Send may be called.
type failingProcessStream struct {
	pb.Processor_ProcessClient
	sends int
}

func (s *failingProcessStream) Send(*pb.ProcessorIn) error {
	s.sends++

	return errors.New("stream broken")
}

// TestFeedCountsTheCopyItCouldNotSend: a copy taken from the queue whose
// Send fails is lost, so it is counted as dropped and feed returns.
func TestFeedCountsTheCopyItCouldNotSend(t *testing.T) {
	p := &Processor{queue: make(chan *pb.Record, 2)}
	p.queue <- &pb.Record{Item: &pb.Record_Sample{Sample: &pb.Sample{Device: "a1", Signal: "count"}}}
	stream := &failingProcessStream{}

	p.feed(context.Background(), stream)

	if stream.sends != 1 {
		t.Fatalf("Send called %d times, want 1", stream.sends)
	}
	if got := p.Stats().Dropped; got != 1 {
		t.Fatalf("Dropped = %d, want 1: the copy taken from the queue is lost", got)
	}
}
