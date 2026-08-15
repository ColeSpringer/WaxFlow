// Package posixfs gives file publishes and the handles around them POSIX
// semantics on every platform. On POSIX it is a thin veneer over os. On
// Windows it closes the gaps that break the temp-then-rename idiom there:
//
//   - Go's os.OpenFile grants only READ|WRITE sharing, under which renaming
//     or deleting an open file fails with ACCESS_DENIED. [Create] and [Open]
//     add FILE_SHARE_DELETE, so a handle behaves like a POSIX fd: it follows
//     a renamed file and keeps reading an unlinked one.
//
//   - os.Rename (MoveFileEx) cannot replace a target any handle holds open.
//     [Replace] renames with FILE_RENAME_POSIX_SEMANTICS (via os.Root.Rename,
//     which falls back internally on filesystems like FAT that lack it), so a
//     replace lands over holders that share delete, i.e. handles from [Open]
//     and [Create]. A holder without delete sharing, such as Defender or the
//     Search Indexer scanning a just-closed file, still blocks any rename;
//     those holds clear in milliseconds, so Replace retries over a short
//     backoff before surfacing the error.
//
// [ReadFile] is the whole-file read built on [Open], for brief reads that
// must never block a concurrent Replace of the same path.
package posixfs
