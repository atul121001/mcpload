//go:build embedengine

package engine

import (
	"embed"
	"io/fs"
)

//go:embed bin/engine
var enginePayload []byte

//go:embed bin/scenarios
var scenariosFS embed.FS

func embeddedScenarios() fs.FS {
	sub, err := fs.Sub(scenariosFS, "bin/scenarios")
	if err != nil {
		return nil
	}
	return sub
}
