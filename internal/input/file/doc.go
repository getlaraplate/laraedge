// Package file is the file input driver: it watches a directory for report
// files written by measuring machines (a CMM, a logger, a gauge) and turns each
// stable file into samples. This part of the package holds the validation of
// the connection and of the points; the watcher, the parsers and the input
// follow.
//
// The package imports time/tzdata so that IANA zone names resolve on Windows
// and on minimal images without a zoneinfo database.
package file

import _ "time/tzdata"
