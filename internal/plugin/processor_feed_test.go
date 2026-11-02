package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
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

	p.feed(context.Background(), stream, nil)

	if stream.sends != 1 {
		t.Fatalf("Send called %d times, want 1", stream.sends)
	}
	if got := p.Stats().Dropped; got != 1 {
		t.Fatalf("Dropped = %d, want 1: the copy taken from the queue is lost", got)
	}
}

// TestFeedEndsAndCountsWhenTheStateCannotBeSent: a Send that fails while
// the state is being sent ends the feed at once, the record it lost counted
// as dropped and neither the rest of the state nor the queue sent.
func TestFeedEndsAndCountsWhenTheStateCannotBeSent(t *testing.T) {
	p := &Processor{queue: make(chan *pb.Record, 2)}
	p.queue <- &pb.Record{Item: &pb.Record_Sample{Sample: &pb.Sample{Device: "a1", Signal: "count"}}}
	state := []model.Record{stateSample("a1", "s1", 1), stateSample("a1", "s2", 2), stateSample("a1", "s3", 3)}
	stream := &failingProcessStream{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.feed(context.Background(), stream, state)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("feed did not return after a failed Send of the state")
	}

	if stream.sends != 1 {
		t.Fatalf("Send called %d times, want 1: the stream is over after the first failure", stream.sends)
	}
	if got := p.Stats().Dropped; got != 1 {
		t.Fatalf("Dropped = %d, want 1: the record of the state the Send lost", got)
	}
	if len(p.queue) != 1 {
		t.Fatalf("%d copies queued, want the queued copy untouched", len(p.queue))
	}
}

// TestStateKeepsNoSampleOfADeadDevice: samples of a device whose last event
// is a death are not kept, so they never count toward the limit; a birth
// makes them kept again.
func TestStateKeepsNoSampleOfADeadDevice(t *testing.T) {
	p := NewProcessor(nil, config.Processor{ID: "p1", Observe: []string{"d1"}}, nil, clock.NewFake(time.Time{}), slog.New(slog.DiscardHandler))
	render := func() []string {
		var out []string
		for _, rec := range p.state() {
			if rec.Event != nil {
				out = append(out, rec.Event.Kind.String()+" "+rec.Event.Device)
			} else {
				out = append(out, fmt.Sprintf("%s/%s=%d", rec.Sample.Device, rec.Sample.Signal, rec.Sample.Value.Int))
			}
		}

		return out
	}

	p.Offer(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "d1"}))
	p.Offer(stateSample("d1", "s1", 1))
	if got, want := render(), []string{"death d1"}; !slices.Equal(got, want) || p.cached != 0 {
		t.Fatalf("state = %q with %d cached, want %q and nothing cached", got, p.cached, want)
	}

	p.Offer(model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "d1", Signals: []model.SignalInfo{{Signal: "s1", DataType: model.DataTypeNumber}}}))
	p.Offer(stateSample("d1", "s1", 2))
	if got, want := render(), []string{"birth d1", "d1/s1=2"}; !slices.Equal(got, want) || p.cached != 1 {
		t.Fatalf("state = %q with %d cached, want %q and one cached", got, p.cached, want)
	}
}

func stateSample(device, signal string, value int64) model.Record {
	return model.SampleRecord(model.Sample{Device: device, Signal: signal, TS: time.Unix(1, 0), Value: model.IntValue(value), Quality: model.Good, Priority: model.Critical})
}
