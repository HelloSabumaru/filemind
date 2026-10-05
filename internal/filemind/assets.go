package filemind

import "embed"

//go:embed web/*.html web/dist/*
var webFiles embed.FS
