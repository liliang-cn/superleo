// Package extension carries the Chrome extension SuperLeo drives the person's
// Chrome through, so a SuperLeo build can unpack it where Chrome can load it.
package extension

import "embed"

// Chrome is the unpacked Chrome extension (extension/chrome).
//
//go:embed chrome
var Chrome embed.FS
