package transform

import (
	"errors"
	"fmt"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// CheckDocument type-checks every point chain of doc, the check config cannot
// make (decision P1): config.Parse validates each parameter, CheckDocument the
// chain as a whole, starting from the kind of the point's decoding type, and
// the bounds only the builders check. Chains are compiled without persistence,
// so nothing is read or written.
//
// It returns one fixed-text message per bad chain, in document order:
//
//	inputs.<i>.devices.<d>.points.<k>.transforms: input <id>, device <name>, signal <name>, step <n> (<transform>): <reason>
//
// An input id, device or signal name is shown only when it has the shape of a
// document key, else as *; the path still locates the chain. It returns nil
// when every chain compiles. Derived signals are not checked: this build
// refuses them (config.CheckBuild).
func CheckDocument(doc config.Document) []string {
	var msgs []string

	for i, in := range doc.Inputs {
		for d, dev := range in.Devices {
			for k, p := range dev.Points {
				if len(p.Transforms) == 0 {
					continue
				}

				if _, err := Compile(p.Transforms, KindOf(p.Type), nil); err != nil {
					msgs = append(msgs, fmt.Sprintf("inputs.%d.devices.%d.points.%d.transforms: input %s, device %s, signal %s, %s",
						i, d, k, shownOrStar(in.ID), shownOrStar(dev.Device), shownOrStar(p.Signal), chainError(err)))
				}
			}
		}
	}

	return msgs
}

// chainError renders a Compile error. Every Compile error is a StepError of
// fixed text; anything else gets a fixed text of its own.
func chainError(err error) string {
	var stepErr *StepError
	if errors.As(err, &stepErr) {
		return stepErr.Error()
	}

	return "the transform chain is invalid"
}

// shownOrStar returns name when it may be named in a message, else "*".
func shownOrStar(name string) string {
	if shown := shownName(name); shown != "" {
		return shown
	}

	return "*"
}

// OutputDataType returns the birth data type of p: the data type of the kind
// its transform chain produces (boolean for a boolean, string for a string,
// number for an integer or a float). With no chain, or a chain that does not
// compile (CheckDocument refuses that document), it is p.DataType().
func OutputDataType(p config.Point) string {
	if len(p.Transforms) == 0 {
		return p.DataType()
	}

	chain, err := Compile(p.Transforms, KindOf(p.Type), nil)
	if err != nil {
		return p.DataType()
	}

	switch chain.OutKind() {
	case model.KindBool:
		return model.DataTypeBoolean
	case model.KindString:
		return model.DataTypeString
	default:
		return model.DataTypeNumber
	}
}
