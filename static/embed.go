package static

import "embed"

//go:embed *.html *.css
var Files embed.FS
