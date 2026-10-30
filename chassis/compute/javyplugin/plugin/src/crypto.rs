//! Native hashing for nano-ops, as `globalThis.__txco_native`.
//!
//! QuickJS interprets JS at roughly 100 ns per operation under wazero, which
//! put the SDK's pure-JS SHA-256 at ~135 KB/s: a 500 ms op could hash about
//! 65 KB. These run as compiled wasm instead, hundreds of times faster.
//!
//!   __txco_native.version                    // 1
//!   __txco_native.sha256(bytes)              // Uint8Array(32)
//!   __txco_native.hmacSha256(key, message)   // Uint8Array(32)
//!
//! Arguments are `Uint8Array`s (a view's offset and length are honoured);
//! anything else throws a TypeError. Strings are the SDK's job to encode.
//! Hex encoding stays in JS: it is 32 bytes.

use hmac::{Hmac, Mac};
use javy_plugin_api::javy::quickjs::{
    Ctx, Error, Object, Result, TypedArray, function::Func,
};
use sha2::{Digest, Sha256};

/// The shape of `__txco_native`; bump when a function is added or changed so
/// the SDK can tell what it has.
const VERSION: u32 = 1;

pub(crate) fn register(ctx: &Ctx<'_>) -> Result<()> {
    let native = Object::new(ctx.clone())?;
    native.set("version", VERSION)?;
    native.set("sha256", Func::from(sha256))?;
    native.set("hmacSha256", Func::from(hmac_sha256))?;
    ctx.globals().set("__txco_native", native)
}

fn bytes<'a, 'js>(arr: &'a TypedArray<'js, u8>) -> Result<&'a [u8]> {
    arr.as_bytes()
        .ok_or_else(|| Error::new_from_js_message("Uint8Array", "bytes", "the buffer is detached"))
}

fn sha256<'js>(ctx: Ctx<'js>, data: TypedArray<'js, u8>) -> Result<TypedArray<'js, u8>> {
    let digest = Sha256::digest(bytes(&data)?);
    TypedArray::<u8>::new_copy(ctx, digest.as_slice())
}

fn hmac_sha256<'js>(
    ctx: Ctx<'js>,
    key: TypedArray<'js, u8>,
    message: TypedArray<'js, u8>,
) -> Result<TypedArray<'js, u8>> {
    // Any key length is valid for HMAC; new_from_slice only fails for an
    // unsupported length, which does not happen with SHA-256.
    let mut mac = Hmac::<Sha256>::new_from_slice(bytes(&key)?)
        .map_err(|e| Error::new_from_js_message("Uint8Array", "hmac key", e.to_string()))?;
    mac.update(bytes(&message)?);
    let tag = mac.finalize().into_bytes();
    TypedArray::<u8>::new_copy(ctx, tag.as_slice())
}
