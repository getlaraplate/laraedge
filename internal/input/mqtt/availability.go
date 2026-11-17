package mqtt

import (
	"slices"
	"sync"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
)

// ErrKindAvailability is the read error kind of an availability payload that is
// neither the configured online nor the offline string.
const ErrKindAvailability = "availability"

// Tracker turns the availability topics of an input into device lifecycle
// calls (spec section 5). A device is held dead from its offline payload until
// its online payload or a sample of it, so KeepAlive, which keeps the devices
// of the input alive while the broker is connected, does not revive it. It is
// safe for concurrent use: the mutex is cheap and keeps a death and the birth
// that follows it in the order they were decided.
type Tracker struct {
	life    *edgeinput.Lifecycle
	devices []string
	topics  map[string][]availabilityRule

	mu   sync.Mutex
	held map[string]bool
	// refused are the devices whose subscriptions the broker refused on the
	// current connection.
	refused map[string]bool
}

type availabilityRule struct {
	device  string
	online  string
	offline string
}

// NewTracker returns the tracker of devices, whose availability topics are
// given by availability (keyed by device). One topic may serve several devices.
func NewTracker(life *edgeinput.Lifecycle, availability map[string]Availability, devices []string) *Tracker {
	t := &Tracker{
		life:    life,
		devices: slices.Clone(devices),
		topics:  make(map[string][]availabilityRule, len(availability)),
		held:    make(map[string]bool),
	}

	names := make([]string, 0, len(availability))
	for device := range availability {
		names = append(names, device)
	}
	slices.Sort(names)

	for _, device := range names {
		a := availability[device]
		t.topics[a.Topic] = append(t.topics[a.Topic], availabilityRule{device: device, online: a.Online, offline: a.Offline})
	}

	return t
}

// Message handles a message of topic. handled is false when topic is not an
// availability topic, so the caller routes it to the decoder. Payloads are
// compared byte for byte with the configured strings, without trimming. A
// payload that is neither state changes nothing and returns ErrKindAvailability.
func (t *Tracker) Message(topic string, payload []byte) (handled bool, errKind string) {
	rules, ok := t.topics[topic]
	if !ok {
		return false, ""
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	value := string(payload)
	for _, r := range rules {
		switch value {
		case r.offline:
			t.offline(r.device)
		case r.online:
			t.online(r.device)
		default:
			errKind = ErrKindAvailability
		}
	}

	return true, errKind
}

// Online releases device and reads it: an online availability payload or a
// Sparkplug birth (decision S3).
func (t *Tracker) Online(device string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.online(device)
}

// Offline holds device dead until Online, Sample or an online availability
// payload: an offline availability payload or a Sparkplug death (decision
// S3). KeepAlive and Seen do not revive it.
func (t *Tracker) Offline(device string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.offline(device)
}

func (t *Tracker) online(device string) {
	delete(t.held, device)
	t.life.Read(device)
}

func (t *Tracker) offline(device string) {
	t.held[device] = true
	t.life.Die(device)
}

// Seen records a Sparkplug sample of device: it reads the device unless it is
// held dead, and reports whether it did. Under Sparkplug a death is
// authoritative until the next birth, so a sample does not revive a device
// (decision S3); the caller drops the samples of a device Seen refuses.
func (t *Tracker) Seen(device string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.held[device] {
		return false
	}
	t.life.Read(device)

	return true
}

// Sample records a sample of device: it is alive, whatever its availability
// last said.
func (t *Tracker) Sample(device string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.held, device)
	t.life.Read(device)
}

// KeepAlive reads every device of the input that is not held dead and not
// refused.
func (t *Tracker) KeepAlive() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, device := range t.devices {
		if !t.held[device] && !t.refused[device] {
			t.life.Read(device)
		}
	}
}

// SetRefused replaces the devices KeepAlive leaves out because the broker
// refused the subscriptions of all their points: nothing can arrive for
// them, so they die after the death tolerance. A sample or an online
// availability payload still revives one.
func (t *Tracker) SetRefused(devices []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.refused = make(map[string]bool, len(devices))
	for _, device := range devices {
		t.refused[device] = true
	}
}

// Topics returns the availability topics, sorted and without duplicates.
func (t *Tracker) Topics() []string {
	topics := make([]string, 0, len(t.topics))
	for topic := range t.topics {
		topics = append(topics, topic)
	}
	slices.Sort(topics)

	return topics
}
