// Package webui embeds the management interface served under /admin.
package webui

import "embed"

// Assets holds index.html, app.js and styles.css. Keeping them embedded means
// the proxy stays a single binary with no external static directory to deploy.
//
//go:embed index.html app.js styles.css
var Assets embed.FS
