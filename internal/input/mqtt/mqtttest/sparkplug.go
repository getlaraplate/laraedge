package mqtttest

import (
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// SparkplugPayload is a Sparkplug B payload for tests. Encode writes it with
// protowire: no Eclipse Tahu code and no generated code.
type SparkplugPayload struct {
	Timestamp, Seq *uint64
	Metrics        []SparkplugMetric
}

// SparkplugMetric is one metric. A nil pointer field is absent.
type SparkplugMetric struct {
	Name             *string
	Alias, Timestamp *uint64
	Datatype         *uint32
	Historical, Null bool
	Quality          *int64
	QualityText      *string
	Int              *uint32
	Long             *uint64
	Float            *float32
	Double           *float64
	Bool             *bool
	String           *string
	Bytes            []byte
}

func varintField(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func bytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

// Encode returns the wire form of the payload.
func (p SparkplugPayload) Encode() []byte {
	var b []byte
	if p.Timestamp != nil {
		b = varintField(b, 1, *p.Timestamp)
	}
	for _, m := range p.Metrics {
		b = bytesField(b, 2, m.encode())
	}
	if p.Seq != nil {
		b = varintField(b, 3, *p.Seq)
	}
	return b
}

func (m SparkplugMetric) encode() []byte {
	var b []byte
	if m.Name != nil {
		b = bytesField(b, 1, []byte(*m.Name))
	}
	if m.Alias != nil {
		b = varintField(b, 2, *m.Alias)
	}
	if m.Timestamp != nil {
		b = varintField(b, 3, *m.Timestamp)
	}
	if m.Datatype != nil {
		b = varintField(b, 4, uint64(*m.Datatype))
	}
	if m.Historical {
		b = varintField(b, 5, 1)
	}
	if m.Null {
		b = varintField(b, 7, 1)
	}
	if m.Quality != nil || m.QualityText != nil {
		b = bytesField(b, 9, m.properties())
	}
	if m.Int != nil {
		b = varintField(b, 10, uint64(*m.Int))
	}
	if m.Long != nil {
		b = varintField(b, 11, *m.Long)
	}
	if m.Float != nil {
		b = protowire.AppendTag(b, 12, protowire.Fixed32Type)
		b = protowire.AppendFixed32(b, float32bits(*m.Float))
	}
	if m.Double != nil {
		b = protowire.AppendTag(b, 13, protowire.Fixed64Type)
		b = protowire.AppendFixed64(b, float64bits(*m.Double))
	}
	if m.Bool != nil {
		v := uint64(0)
		if *m.Bool {
			v = 1
		}
		b = varintField(b, 14, v)
	}
	if m.String != nil {
		b = bytesField(b, 15, []byte(*m.String))
	}
	if m.Bytes != nil {
		b = bytesField(b, 16, m.Bytes)
	}
	return b
}

// properties encodes a PropertySet holding the Quality property: an int_value
// (or long_value when it does not fit 32 bits), or a string_value for
// QualityText.
func (m SparkplugMetric) properties() []byte {
	var value []byte
	if m.QualityText != nil {
		value = varintField(value, 1, 12)
		value = bytesField(value, 8, []byte(*m.QualityText))
	} else if q := *m.Quality; q >= 0 && q <= 0xFFFFFFFF {
		value = varintField(value, 1, 3)
		value = varintField(value, 3, uint64(q))
	} else {
		value = varintField(value, 1, 4)
		value = varintField(value, 4, uint64(q))
	}
	var set []byte
	set = bytesField(set, 1, []byte("Quality"))
	set = bytesField(set, 2, value)
	return set
}

// BdSeq is the bdSeq metric of a birth or death: Int64 in long_value.
func BdSeq(v uint64) SparkplugMetric {
	name := "bdSeq"
	dt := uint32(4)
	return SparkplugMetric{Name: &name, Datatype: &dt, Long: &v}
}

func float32bits(f float32) uint32 { return math.Float32bits(f) }
func float64bits(f float64) uint64 { return math.Float64bits(f) }
