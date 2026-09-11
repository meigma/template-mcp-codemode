package reloader

// ChangeEvent is one raw source-change notification from a Watcher.
//
// It is deliberately minimal: the core only needs to know that something
// changed in order to debounce and rebuild. Later milestones may extend it
// as the debounce logic firms up.
type ChangeEvent struct {
	// Path is the filesystem path that changed.
	Path string
}

// BuildResult describes one successful build cycle.
type BuildResult struct {
	// Artifact is the unique per-cycle path of the built child binary,
	// never the running child's path: a running binary is never
	// overwritten in place.
	Artifact string

	// Output is the build's compile output, surfaced to the developer.
	Output string
}
