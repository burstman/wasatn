// Package wasatn holds assets that must be compiled into the binary.
//
// Static files live in /static so the Tailwind CLI and templ tooling can find
// them by conventional paths; embedding them here means the deployed binary has
// no runtime dependency on the working directory.
package wasatn

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var staticAssets embed.FS

// StaticFS returns the built asset tree rooted at the /static prefix, so
// "css/app.css" serves /static/css/app.css.
func StaticFS() (fs.FS, error) {
	return fs.Sub(staticAssets, "static")
}
