// Package watch is the fsnotify-backed file watching adapter for the dev
// proxy.
//
// It implements the reloader.Watcher port: each configured directory is
// watched recursively (fsnotify watches single directories, so the adapter
// walks the tree and also registers newly created subdirectories as they
// appear) and every relevant filesystem event is forwarded as a raw
// reloader.ChangeEvent. Debouncing and coalescing are deliberately absent
// here — they are core logic owned by the reloader package.
package watch
