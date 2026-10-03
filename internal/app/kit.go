package app

import "github.com/anthdm/superkit/kit"

// KitUseErrorHandler installs the app's central error handler.
//
// It exists so cmd binaries do not need to import SuperKit directly, and it is
// called exactly once during boot because SuperKit stores the handler in
// package-level state.
func KitUseErrorHandler(h kit.ErrorHandlerFunc) {
	kit.UseErrorHandler(h)
}
