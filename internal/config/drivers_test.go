package config

// Load resolves SUXEN_BLOBSTORE against the compiled-in blob driver registry,
// so the tests link the built-in drivers exactly like a real binary does.
import _ "github.com/suxen-project/suxen/internal/blob"
