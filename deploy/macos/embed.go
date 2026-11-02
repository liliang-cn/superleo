// Package macos carries the LaunchAgent template for the watchdog.
package macos

import _ "embed"

// WatchdogPlist is deploy/macos/cn.superleo.watchdog.plist, a text/template
// with {{.Binary}}, {{.Home}} and {{.LogDir}}.
//
//go:embed cn.superleo.watchdog.plist
var WatchdogPlist string
