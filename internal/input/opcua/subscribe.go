// This file holds the subscription path of the OPC UA input: monitored-item requests, the session subscription and its notification dispatch, and the input side that consumes them.

package opcua

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// monitorRequest is the monitored item of it (O9): Value attribute, reporting,
// sampling in milliseconds (-1: the publishing interval), queue (0: 10),
// discard oldest, a DataChangeFilter when a deadband is set.
func monitorRequest(it item) (*ua.MonitoredItemCreateRequest, error) {
	kind, value, err := ParseDeadband(it.Sub.Deadband)
	if err != nil {
		return nil, err
	}

	req := opcua.NewMonitoredItemCreateRequestWithDefaults(it.Node, ua.AttributeIDValue, it.Handle)
	params := req.RequestedParameters

	params.SamplingInterval = -1
	if it.Sub.Sampling != config.SamplingDefault {
		params.SamplingInterval = float64(it.Sub.Sampling) / float64(time.Millisecond)
	}

	params.QueueSize = 10
	if it.Sub.Queue != 0 {
		params.QueueSize = it.Sub.Queue
	}

	params.DiscardOldest = true

	if kind != uint32(ua.DeadbandTypeNone) {
		params.Filter = ua.NewExtensionObject(&ua.DataChangeFilter{
			Trigger:       ua.DataChangeTriggerStatusValue,
			DeadbandType:  kind,
			DeadbandValue: value,
		})
	}

	return req, nil
}

func (s *uaSession) Subscribe(ctx context.Context, interval time.Duration, items []item) (<-chan notification, []ua.StatusCode, error) {
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("opcua subscribe: %w", errNoItems)
	}

	reqs := make([]*ua.MonitoredItemCreateRequest, len(items))
	for i, it := range items {
		req, err := monitorRequest(it)
		if err != nil {
			return nil, nil, fmt.Errorf("opcua subscribe: %w", err)
		}
		reqs[i] = req
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()

		return nil, nil, fmt.Errorf("opcua subscribe: %w", errClosed)
	case s.subscribed:
		s.mu.Unlock()

		return nil, nil, fmt.Errorf("opcua subscribe: %w", errSubscribed)
	}
	s.subscribed = true
	s.wg.Add(1)
	s.mu.Unlock()

	in := make(chan *opcua.PublishNotificationData, notifyBuffer)
	out := make(chan notification, notifyBuffer)

	go s.drain(in, out)

	var sub uaSubscription

	err := contain(func() (err error) {
		sub, err = s.client.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: interval, MaxKeepAliveCount: s.t.MaxKeepAliveCount, LifetimeCount: s.t.lifetime(interval)}, in)

		return err
	})
	if err != nil {
		return nil, nil, classify(ctx, s.log, s.inputID, "subscribe", err)
	}

	statuses := make([]ua.StatusCode, 0, len(items))

	for chunk := range slices.Chunk(reqs, s.maxMonitored) {
		var res *ua.CreateMonitoredItemsResponse

		err := contain(func() (err error) {
			res, err = sub.Monitor(ctx, ua.TimestampsToReturnBoth, chunk...)

			return err
		})
		if err != nil {
			return nil, nil, classify(ctx, s.log, s.inputID, "create monitored items", err)
		}
		if res == nil || len(res.Results) != len(chunk) {
			return nil, nil, fmt.Errorf("opcua create monitored items: %w", errMalformed)
		}

		for _, r := range res.Results {
			if r == nil {
				statuses = append(statuses, ua.StatusBad)

				continue
			}
			statuses = append(statuses, r.StatusCode)
		}
	}

	return out, statuses, nil
}

// drain empties the library's channel until Close, so the library's publish
// loop never blocks on it, and forwards the data changes. A status change or
// a publish fault means the subscription is gone: OnLost.
func (s *uaSession) drain(in <-chan *opcua.PublishNotificationData, out chan<- notification) {
	defer s.wg.Done()
	defer close(out)

	for {
		select {
		case <-s.done:
			return
		case msg := <-in:
			if !s.dispatch(msg, out) {
				return
			}
		}
	}
}

// dispatch handles one message of the library; false when Close interrupted.
func (s *uaSession) dispatch(msg *opcua.PublishNotificationData, out chan<- notification) bool {
	if msg == nil {
		return true
	}

	if msg.Error != nil {
		var status ua.StatusCode
		if errors.As(msg.Error, &status) && uint32(status)>>30 != 0 {
			s.log.Debug("opcua: subscription lost", "input", s.inputID, "error", msg.Error.Error())
			s.watch.signal()

			return true
		}

		s.log.Debug("opcua: publish message ignored", "input", s.inputID, "error", msg.Error.Error())

		return true
	}

	switch v := msg.Value.(type) {
	case *ua.DataChangeNotification:
		s.messages++

		for _, mi := range v.MonitoredItems {
			if mi == nil {
				continue
			}

			select {
			case out <- notification{Handle: mi.ClientHandle, Value: mi.Value, Message: s.messages}:
			case <-s.done:
				return false
			}
		}
	case *ua.StatusChangeNotification:
		s.log.Debug("opcua: subscription status changed", "input", s.inputID, "status", v.Status.Error())
		s.watch.signal()
	}

	return true
}

// subscribe creates the session's subscription for the resolved subscribe
// points and starts the reader of its notifications. A monitored item the
// server refuses gets one bad sample, and kind exception counts once for the
// response (ruling R8); the item is created again at the next connect only.
func (in *Input) subscribe(ctx context.Context, s session, ids []*ua.NodeID, l *link, reader *sync.WaitGroup) error {
	var items []item

	for _, i := range in.subscribed {
		if ids[i] == nil {
			continue
		}

		sub := config.Subscribe{Sampling: config.SamplingDefault}
		if in.points[i].cfg.Read.Subscribe != nil {
			sub = *in.points[i].cfg.Read.Subscribe
		}

		items = append(items, item{Handle: uint32(i), Node: ids[i], Sub: sub})
	}

	if len(items) == 0 {
		return nil
	}

	notifications, statuses, err := s.Subscribe(ctx, in.conn.PublishingInterval, items)
	if err != nil {
		return err
	}

	now := in.clk.Now()
	refused := false

	for k, status := range statuses {
		if Quality(status) == model.Bad {
			refused = true
			l.setup = append(l.setup, in.badSample(int(items[k].Handle), now))
		}
	}

	if refused {
		in.health.CountError(input.ReadErrorException)
	}

	// Started after l.setup is complete. The reader never blocks on anything
	// but emit, so the session's drain never stalls the library's publish
	// loop on I/O (Task 4).
	reader.Go(func() {
		var counted uint64

		for n := range notifications {
			if ctx.Err() == nil {
				in.notify(n, l, &counted)
			}
		}
	})

	return nil
}

// notify emits the sample of one data change; a notification is a
// successful exchange (O4). A Bad status counts kind exception once per
// publish message (ruling R8): counted is the last message counted.
//
// A notification for a handle the input never gave is dropped and does not
// count as an exchange: it is not an answer about any point of the input, and
// a server that sends only such notifications is still checked by the
// watchdog read, which keeps the devices alive while the server answers.
func (in *Input) notify(n notification, l *link, counted *uint64) {
	if int(n.Handle) >= len(in.points) {
		in.log.Debug("opcua: notification for an unknown handle ignored", "input", in.cfg.ID, "handle", n.Handle)

		return
	}

	in.alive(l)

	s, kind := in.sample(int(n.Handle), n.Value)
	switch {
	case kind == input.ReadErrorDecode:
		in.health.CountError(kind)
	case kind == input.ReadErrorException && (n.Message == 0 || n.Message != *counted):
		in.health.CountError(kind)
		*counted = n.Message
	}
	l.emit(model.SampleRecord(s))
}
