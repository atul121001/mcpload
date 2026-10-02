package client

import (
	"encoding/json"
	"sync"
)

// Resolution is the outcome of a successful "auto" protocol negotiation.
type Resolution struct {
	Protocol     string
	Stateless    bool
	Capabilities json.RawMessage
	ServerInfo   json.RawMessage
}

// ProtocolCache remembers the protocol that "auto" resolved to, keyed by
// server URL and the requested protocol options, so later connects (from any
// VU sharing the cache) skip the server/discover probe. Only successful
// negotiations are stored. Safe for concurrent use.
type ProtocolCache struct {
	m sync.Map // protoKey -> Resolution
}

type protoKey struct {
	url, protocol, fallback string
}

// SharedProtocolCache is the process-wide cache used by the k6 module.
var SharedProtocolCache = &ProtocolCache{}

// Load returns the cached resolution for opts, if any.
func (c *ProtocolCache) Load(url, protocol, fallback string) (Resolution, bool) {
	v, ok := c.m.Load(protoKey{url, protocol, fallback})
	if !ok {
		return Resolution{}, false
	}
	return v.(Resolution), true
}

// Store records a successful resolution.
func (c *ProtocolCache) Store(url, protocol, fallback string, r Resolution) {
	c.m.Store(protoKey{url, protocol, fallback}, r)
}

// Forget drops the cached resolution (e.g. after the server stopped
// accepting the remembered protocol).
func (c *ProtocolCache) Forget(url, protocol, fallback string) {
	c.m.Delete(protoKey{url, protocol, fallback})
}
