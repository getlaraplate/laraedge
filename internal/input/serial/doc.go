// Package serial is the serial input driver: it reads frames from a machine
// connected to a serial port (a gauge, a scanner, a multiplexer) and turns them
// into samples. The design is in docs/superpowers/specs/2026-11-19-serial-driver-design.md.
//
// This part of the package holds the port: opening, the error kinds the driver
// tells apart, and the listing of the ports of the host.
package serial
