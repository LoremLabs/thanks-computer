//! The txco Javy plugin.
//!
//! This is Javy's default plugin (`crates/plugin` in the Javy repository at
//! the version `javyplugin.JavyVersion` pins) with one addition: the natives
//! in `crypto.rs`, registered on the global object in `modify_runtime`. The
//! `@txco/op/crypto` SDK calls them when present and falls back to its
//! pure-JS implementation otherwise. Everything else — the config read from
//! stdin, the enabled builtins, the exports Javy needs — is unchanged, so an
//! op compiled against this plugin behaves exactly as one compiled against
//! the default plugin, only faster where it hashes.
//!
//! The namespace below is the wasm import module name a dynamically linked
//! op uses to reach this plugin. It is part of the compiled op's identity:
//! change it whenever the plugin's QuickJS build or globals change in a way
//! an existing op's bytecode could notice, and keep the previous plugin
//! vendored so ops already in a store still link (see javyplugin.go).

use std::io::{self, Read};

use javy_plugin_api::javy::Runtime;
use javy_plugin_api::{Config, import_namespace};

use crate::shared_config::SharedConfig;

mod crypto;
mod shared_config;

import_namespace!("txco-javy-plugin-v1");

fn config() -> Config {
    // Read shared config JSON in from stdin.
    // Using stdin instead of an environment variable because the value set for
    // an environment variable will persist as the value set for that environment
    // variable in subsequent invocations so a different value can't be used to
    // initialize a runtime with a different configuration.
    let mut config = Config::default();
    config
        .text_encoding(true)
        .javy_stream_io(true)
        .simd_json_builtins(true);

    let mut config_bytes = vec![];
    let shared_config = match io::stdin().read_to_end(&mut config_bytes) {
        Ok(0) => None,
        Ok(_) => Some(SharedConfig::parse_from_json(&config_bytes).unwrap()),
        Err(e) => panic!("Error reading from stdin: {e}"),
    };
    if let Some(shared_config) = shared_config {
        shared_config.apply_to_config(&mut config);
    }
    config
}

fn modify_runtime(runtime: Runtime) -> Runtime {
    runtime.context().with(|ctx| {
        crypto::register(&ctx).expect("register txco natives");
    });
    runtime
}

#[unsafe(export_name = "initialize-runtime")]
fn initialize_runtime() {
    javy_plugin_api::initialize_runtime(config, modify_runtime).unwrap();
}
