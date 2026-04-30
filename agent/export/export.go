// Package export provides session export functionality to HTML, JSON, and Markdown formats.
package export

import (
	"embed"
	"io/fs"
)

//go:embed templates/*
var templates embed.FS

// CSS returns the embedded CSS stylesheet.
func CSS() (string, error) {
	data, err := templates.ReadFile("templates/style.css")
	return string(data), err
}

// GetTemplates returns the templates filesystem for direct access.
func GetTemplates() fs.FS {
	sub, err := fs.Sub(templates, "templates")
	if err != nil {
		return templates
	}
	return sub
}
