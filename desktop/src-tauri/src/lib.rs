// Desktop wrapper: starts `llm-agent-spend-manager serve` locally and shows the same
// embedded dashboard in a native window (the window URL is set in
// tauri.conf.json to http://127.0.0.1:4600).
//
// The Go binary is the source of truth for the dashboard and the data; this
// wrapper only launches it and hosts the WebView, so the desktop app never
// drifts from the CLI/PWA.

use std::process::{Child, Command};
use std::sync::Mutex;
use tauri::Manager;

const PORT: &str = "4600";

// Keep the child handle alive for the app's lifetime and kill it on exit.
struct Server(Mutex<Option<Child>>);

fn spawn_server() -> std::io::Result<Child> {
    // Resolve the binary: prefer one shipped next to the app, fall back to PATH.
    let bin = which_binary();
    Command::new(bin)
        .args(["serve", "--local", "--port", PORT])
        .spawn()
}

fn which_binary() -> String {
    // Bundled alongside the desktop app (see README) or on PATH.
    let name = if cfg!(windows) {
        "llm-agent-spend-manager.exe"
    } else {
        "llm-agent-spend-manager"
    };
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            let candidate = dir.join(name);
            if candidate.exists() {
                return candidate.to_string_lossy().into_owned();
            }
        }
    }
    name.to_string()
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            match spawn_server() {
                Ok(child) => app.manage(Server(Mutex::new(Some(child)))),
                Err(e) => eprintln!("could not start llm-agent-spend-manager server: {e}"),
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            // Best-effort: stop the spawned server when the last window closes.
            if let tauri::WindowEvent::Destroyed = event {
                if let Some(state) = window.app_handle().try_state::<Server>() {
                    if let Ok(mut guard) = state.0.lock() {
                        if let Some(mut child) = guard.take() {
                            let _ = child.kill();
                        }
                    }
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
