package serial

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/getlaraplate/laraedge/internal/config"
)

// Browse lists the serial ports of the host, one line per port sorted by
// name, as "<name>  <stable path or ->  <vid:pid or ->  <product>  <serial>"
// (an empty product or serial is "-"). The port the input's device names, by
// Name or by StablePath, is prefixed "* ", the others two spaces; when none
// matches, "device <d>: not found" ends the list. Values come from the host's
// USB descriptors and are escaped like any text from a device.
func Browse(cfg config.Input, w io.Writer, list func() ([]PortInfo, error)) error {
	conn, err := ParseConnection(cfg.Connection)
	if err != nil {
		return fmt.Errorf("serial input %q: %w", cfg.ID, err)
	}
	if list == nil {
		list = ListPorts
	}
	ports, err := list()
	if err != nil {
		return fmt.Errorf("serial input %q: %w", cfg.ID, err)
	}
	ports = slices.Clone(ports)
	slices.SortStableFunc(ports, func(a, b PortInfo) int { return strings.Compare(a.Name, b.Name) })

	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return escapeText(s)
	}
	found := false
	for _, p := range ports {
		mark := "  "
		if p.Name == conn.Device || (p.StablePath != "" && p.StablePath == conn.Device) {
			mark, found = "* ", true
		}
		ids := "-"
		if p.VID != "" || p.PID != "" {
			ids = p.VID + ":" + p.PID
		}
		if _, err := fmt.Fprintf(w, "%s%s  %s  %s  %s  %s\n", mark, escapeText(p.Name), dash(p.StablePath), ids, dash(p.Product), dash(p.SerialNumber)); err != nil {
			return err
		}
	}
	if !found {
		if _, err := fmt.Fprintf(w, "device %s: not found\n", escapeText(conn.Device)); err != nil {
			return err
		}
	}
	return nil
}

// escapeText makes s safe to print on one line.
func escapeText(s string) string {
	q := fmt.Sprintf("%q", s)
	q = q[1 : len(q)-1]
	return strings.ReplaceAll(q, `\"`, `"`)
}
