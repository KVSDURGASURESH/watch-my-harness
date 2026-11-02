// Package tokendashboard exposes the files the single-binary build ships:
// the vanilla-JS UI, the default pricing table and the licence notices.
package tokendashboard

import "embed"

//go:embed web pricing.json LICENSE NOTICE
var Assets embed.FS
