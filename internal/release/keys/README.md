# Release keys

The minisign public keys pperp checks its releases with (`pperp upgrade`), built into every binary: `pperp.pub`, and
`pperp-next.pub` while a new key is rolled out. They are written by `go run ./tools/release keygen` on the computer
that keeps the secret key (never this repository, never a server or CI).
