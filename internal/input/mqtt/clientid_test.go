package mqtt

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/getlaraplate/laraedge/internal/config"
)

// clientIDInput is an mqtt input id with the given connection.
func clientIDInput(id, conn string) config.Input {
	return config.Input{
		ID:         id,
		Driver:     config.DriverMQTT,
		Connection: json.RawMessage(conn),
		Devices:    []config.Device{device("d-"+id, subscribePoint("v", "plant/"+id, "float64"))},
	}
}

func TestCheckClientIDs(t *testing.T) {
	const want = `inputs.1.connection.client_id: input "b" has the same broker and client id as input "a" (inputs.0); every mqtt input needs its own client id on a broker`

	tests := []struct {
		name   string
		inputs []config.Input
		want   []string
	}{
		{"defaults never collide", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b"}`),
			clientIDInput("b", `{"broker":"mqtt://b"}`),
		}, nil},
		{"same explicit id, same broker", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://b","client_id":"gw"}`),
		}, []string{want}},
		{"default port filled in", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://b:1883","client_id":"gw"}`),
		}, []string{want}},
		{"host letter case", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://Broker.local","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://broker.LOCAL","client_id":"gw"}`),
		}, []string{want}},
		{"explicit id equal to a default id", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b"}`),
			clientIDInput("b", `{"broker":"mqtt://b","client_id":"laraplate-edge-agent-7-a"}`),
		}, []string{want}},
		{"other port", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://b:1884","client_id":"gw"}`),
		}, nil},
		{"other scheme", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b:8883","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtts://b","client_id":"gw"}`),
		}, nil},
		{"other host", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://c","client_id":"gw"}`),
		}, nil},
		{"other driver ignored", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			{ID: "b", Driver: config.DriverSimulated, Connection: json.RawMessage(`{"broker":"mqtt://b","client_id":"gw"}`)},
		}, nil},
		{"an unparsable connection is left to the factory", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://b","client_id":"gw","bogus":1}`),
		}, nil},
		{"three inputs name the first", []config.Input{
			clientIDInput("a", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("b", `{"broker":"mqtt://b","client_id":"gw"}`),
			clientIDInput("c", `{"broker":"mqtt://b","client_id":"gw"}`),
		}, []string{
			want,
			`inputs.2.connection.client_id: input "c" has the same broker and client id as input "a" (inputs.0); every mqtt input needs its own client id on a broker`,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckClientIDs(config.Document{Inputs: tt.inputs}, "agent-7")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CheckClientIDs =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
