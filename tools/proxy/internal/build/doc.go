// Package build is the exec-backed build adapter for the dev proxy.
//
// It implements the reloader.Builder port: each Build runs the configured
// build command once, substituting {{artifact}} with a unique per-cycle
// artifact path inside a Builder-owned temp directory. The running child's
// binary is never overwritten in place (ETXTBSY on Linux; macOS codesign
// invalidation can SIGKILL the running process). Compile output is captured
// and surfaced to the developer — on the BuildResult for successful builds,
// attached to the returned error for failed ones.
package build
