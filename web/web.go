// Package web embeds the UI assets so the binary stays self-contained: no CDN,
// no webfont request, nothing for the user to install.
package web

import "embed"

//go:embed index.html style.css connect.js fonts
var FS embed.FS
