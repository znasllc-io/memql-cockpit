//go:build gui && !darwin

package worker

// preflightScreenCaptureAccess and requestScreenCaptureAccess are
// macOS-only -- there's no equivalent system-level TCC ledger on
// Linux or Windows. The non-darwin gui builds always report "true"
// for both so the wizard treats the screen-recording step as
// granted-by-default; downstream capture failures surface as
// runtime errors rather than pre-flight denials.
func preflightScreenCaptureAccess() bool { return true }
func requestScreenCaptureAccess() bool   { return true }
