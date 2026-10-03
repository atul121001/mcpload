//go:build !embedengine

package engine

import "io/fs"

// Builds without the embedengine tag (go build, go test) carry no engine.
var enginePayload []byte

func embeddedScenarios() fs.FS { return nil }
