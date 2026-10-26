package web

import "embed"

//go:embed all:dist
var dist embed.FS

func Dist() embed.FS {
	return dist
}
