package mqtt

import (
	"bytes"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
)

func FuzzDecodePayload(f *testing.F) {
	full := mqtttest.SparkplugPayload{
		Timestamp: ptr(uint64(1700000000000)),
		Seq:       ptr(uint64(1)),
		Metrics: []mqtttest.SparkplugMetric{
			{Name: ptr("a"), Alias: ptr(uint64(1)), Datatype: ptr(uint32(3)), Int: ptr(uint32(5)), Quality: ptr(int64(192))},
			{Name: ptr("b"), Datatype: ptr(uint32(10)), Double: ptr(1.5)},
			{Name: ptr("c"), Datatype: ptr(uint32(12)), String: ptr("x")},
			mqtttest.BdSeq(1),
		},
	}
	f.Add(full.Encode())
	f.Add(encodeRebirth(time.UnixMilli(1700000000000)))
	f.Add([]byte(nil))
	f.Add([]byte{0x0a})
	f.Add(bytes.Repeat([]byte{0xff}, 16))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := decodePayload(b)
		if err != nil {
			return
		}
		for _, m := range p.Metrics {
			metricValue(m, m.Datatype, "int64")
			metricQuality(m)
			metricTime(m, p, time.Unix(0, 0))
		}
	})
}
