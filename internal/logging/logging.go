// Package logging builds the application logger.
//
// Everything the process says goes through log/slog. JSON is the deployment
// format, because logs are read by machines before they are read by people;
// the text handler exists so that a local terminal stays readable.
package logging

import (
	"io"
	"log/slog"
)

// Format names a slog handler.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// New returns a logger writing to w. Any format other than FormatText yields
// the JSON handler, so an unexpected value degrades to the production format
// rather than to no logging at all.
func New(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if format == FormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}
