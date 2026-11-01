// Package configs embeds the JSON Schemas for seed and config files so the
// binary validates without the source tree.
package configs

import "embed"

// Schemas holds schema/*.json.
//
//go:embed schema/*.json
var Schemas embed.FS
