# txco Javy plugin

The QuickJS engine every `op://` nano-op links against, vendored next to this
directory as `txco-javy-plugin-v1.wasm`. It is Javy's default plugin plus the
natives in `src/crypto.rs` (`__txco_native.sha256`, `.hmacSha256`), which the
`@txco/op/crypto` SDK uses when present.

## Rebuilding

Only needed when this crate changes or Javy is bumped. From the repo root:

```sh
make plugin
```

which runs `../build.sh`: `cargo build --target wasm32-wasip1 --release` with
the toolchain pinned in `rust-toolchain.toml` (rustup installs it and the
target on first use; the QuickJS C sources need a WASI SDK, which the
`rquickjs-sys` build script downloads on its own), then `javy init-plugin` with
the pinned Javy (`TXCO_JAVY`, `javy` on PATH, or the copy `txco` downloaded
into its tools directory). The result replaces the vendored `.wasm`.

Keep `javy-plugin-api` at the version the pinned Javy CLI ships
(`chassis/compute/javyplugin.JavyVersion`): the CLI runs this plugin to
compile each op to bytecode, and a mismatch produces bytecode the runtime's
copy cannot decode. After a rebuild run the op-build and compute tests, and
regenerate the fixture under `chassis/compute/wazero/testdata/src/js`.

## Changing the namespace

`import_namespace!` in `src/lib.rs` names the wasm import module compiled ops
use. An op's bytecode is specific to the plugin build, so when a change could
affect existing ops, bump the namespace (`-v2`), register the new plugin in
`javyplugin.go`, and keep the previous `.wasm` vendored: ops already in a
store and prebuilt packages keep linking against it until they are rebuilt.
