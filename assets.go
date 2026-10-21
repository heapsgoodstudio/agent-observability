// Package agentobservability carries the files claude-obs install places in an
// install dir, so the binary installs from wherever it lives, such as a
// Homebrew prefix, with no source checkout beside it.
package agentobservability

import "embed"

// Assets holds bin/, notifier/ and config.env.example as they are in this
// checkout. Embedding drops file modes; installer.CopyTree makes every ".sh"
// file executable.
//
//go:embed bin notifier config.env.example
var Assets embed.FS
