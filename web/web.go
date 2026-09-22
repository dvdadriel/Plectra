// Package web embeds the UI assets so the binary stays self-contained.
package web

import "embed"

//go:embed index.html
var FS embed.FS
