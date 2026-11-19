// Package buildinfo records immutable release metadata in both optional helpers.
package buildinfo

import "fmt"

var Version = "dev"
var Revision = "unknown"

func String(name string) string { return fmt.Sprintf("%s %s (%s)", name, Version, Revision) }
