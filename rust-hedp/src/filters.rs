//! MiniJinja filters that reproduce the Helm/sprig functions the charts use.
//!
//! Every one of these has to match its Go counterpart byte for byte, because
//! the whole point of this port is a comparison, and a comparison between two
//! programs that emit different bytes measures nothing. `to_yaml` is the
//! finicky one: Helm's `toYaml` goes through sigs.k8s.io/yaml, which marshals
//! via JSON and therefore sorts map keys and quotes strings that would
//! otherwise parse as another type.

use minijinja::value::{Value, ValueKind};
use minijinja::{Error, ErrorKind};
use sha2::{Digest, Sha256};

/// `nindent` prepends a newline then indents every line by n spaces - Helm's
/// idiom for splicing a rendered block into a YAML document.
pub fn nindent(value: String, n: usize) -> String {
    let pad = " ".repeat(n);
    let mut out = String::with_capacity(value.len() + n * 8 + 1);
    out.push('\n');
    for (i, line) in value.split('\n').enumerate() {
        if i > 0 {
            out.push('\n');
        }
        out.push_str(&pad);
        out.push_str(line);
    }
    out
}

/// `indent` is `nindent` without the leading newline.
pub fn indent_filter(value: String, n: usize) -> String {
    let pad = " ".repeat(n);
    let mut out = String::with_capacity(value.len() + n * 8);
    for (i, line) in value.split('\n').enumerate() {
        if i > 0 {
            out.push('\n');
        }
        out.push_str(&pad);
        out.push_str(line);
    }
    out
}

/// `quote` wraps in double quotes, matching sprig's behaviour of quoting the
/// string form of whatever it is given.
pub fn quote(value: Value) -> String {
    match value.kind() {
        ValueKind::Undefined | ValueKind::None => "\"\"".to_string(),
        _ => format!("\"{}\"", value),
    }
}

pub fn sha256sum(value: String) -> String {
    let mut hasher = Sha256::new();
    hasher.update(value.as_bytes());
    hex::encode(hasher.finalize())
}

/// `trunc` keeps the first n characters. Go's sprig operates on bytes; these
/// templates only ever feed it ASCII, so the distinction does not bite, but
/// truncating on a char boundary is the safe form.
pub fn trunc(value: String, n: usize) -> String {
    if value.chars().count() <= n {
        return value;
    }
    value.chars().take(n).collect()
}

pub fn trim_suffix(value: String, suffix: String) -> String {
    value
        .strip_suffix(&suffix)
        .map(|s| s.to_string())
        .unwrap_or(value)
}

pub fn basename(value: String) -> String {
    value
        .rsplit('/')
        .next()
        .map(|s| s.to_string())
        .unwrap_or(value)
}

/// `to_yaml` mirrors Helm's `toYaml`: block-style YAML with sorted keys and no
/// trailing newline.
///
/// It is written by hand rather than delegated to a YAML crate because the
/// exact output - how strings get quoted, how nested maps indent, how empty
/// collections render - is what the equivalence test compares, and no crate
/// matches sigs.k8s.io/yaml's choices out of the box.
pub fn to_yaml(value: Value) -> Result<String, Error> {
    let json: serde_json::Value = serde_json::to_value(&value)
        .map_err(|e| Error::new(ErrorKind::InvalidOperation, format!("to_yaml: {e}")))?;
    let mut out = String::new();
    emit(&json, 0, &mut out, false);
    Ok(out.trim_end_matches('\n').to_string())
}

fn emit(v: &serde_json::Value, indent: usize, out: &mut String, in_seq: bool) {
    use serde_json::Value as J;
    let pad = " ".repeat(indent);
    match v {
        J::Object(map) if map.is_empty() => out.push_str("{}\n"),
        J::Object(map) => {
            // serde_json::Map is a BTreeMap without the preserve_order feature,
            // so iteration is already sorted - the same order sigs.k8s.io/yaml
            // produces by going through encoding/json.
            for (i, (k, val)) in map.iter().enumerate() {
                if i > 0 || !in_seq {
                    out.push_str(&pad);
                }
                out.push_str(&escape_key(k));
                out.push(':');
                match val {
                    J::Object(m) if !m.is_empty() => {
                        out.push('\n');
                        emit(val, indent + 2, out, false);
                    }
                    J::Array(a) if !a.is_empty() => {
                        out.push('\n');
                        emit(val, indent, out, false);
                    }
                    _ => {
                        out.push(' ');
                        emit(val, indent, out, false);
                    }
                }
            }
        }
        J::Array(arr) if arr.is_empty() => out.push_str("[]\n"),
        J::Array(arr) => {
            for item in arr {
                out.push_str(&pad);
                out.push_str("- ");
                match item {
                    J::Object(m) if !m.is_empty() => emit(item, indent + 2, out, true),
                    J::Array(a) if !a.is_empty() => emit(item, indent + 2, out, true),
                    _ => emit(item, 0, out, false),
                }
            }
        }
        J::String(s) => {
            out.push_str(&scalar(s));
            out.push('\n');
        }
        J::Number(n) => {
            out.push_str(&n.to_string());
            out.push('\n');
        }
        J::Bool(b) => {
            out.push_str(if *b { "true" } else { "false" });
            out.push('\n');
        }
        J::Null => out.push_str("null\n"),
    }
}

fn escape_key(k: &str) -> String {
    if needs_quotes(k) {
        format!("{:?}", k)
    } else {
        k.to_string()
    }
}

/// scalar decides whether a string needs quoting. A YAML string that would
/// otherwise parse as a number, a bool, or null has to be quoted or it changes
/// type on the way back in - which is why Helm emits `cpu: "1"` and not
/// `cpu: 1`.
fn scalar(s: &str) -> String {
    if s.is_empty() {
        return "\"\"".to_string();
    }
    if needs_quotes(s) || parses_as_other_type(s) {
        return format!("{:?}", s);
    }
    s.to_string()
}

fn needs_quotes(s: &str) -> bool {
    s.is_empty()
        || s.starts_with(' ')
        || s.ends_with(' ')
        || s.contains('\n')
        || s.contains('"')
        || s.contains('\'')
        || s.contains(": ")
        || s.contains(" #")
        || s.starts_with(['&', '*', '!', '|', '>', '%', '@', '`', '{', '[', '?', ',', '-'])
        || s.ends_with(':')
}

fn parses_as_other_type(s: &str) -> bool {
    if s.parse::<f64>().is_ok() || s.parse::<i64>().is_ok() {
        return true;
    }
    matches!(
        s.to_ascii_lowercase().as_str(),
        "true" | "false" | "null" | "~" | "yes" | "no" | "on" | "off"
    )
}

/// register wires every filter into an Environment.
pub fn register(env: &mut minijinja::Environment<'static>) {
    env.add_filter("nindent", nindent);
    env.add_filter("indent", indent_filter);
    env.add_filter("quote", quote);
    env.add_filter("sha256sum", sha256sum);
    env.add_filter("trunc", trunc);
    env.add_filter("trim_suffix", trim_suffix);
    env.add_filter("basename", basename);
    env.add_filter("to_yaml", to_yaml);
}
