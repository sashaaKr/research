//! The filters carry the byte-for-byte compatibility with Helm's sprig
//! functions, so they get direct tests rather than only being exercised
//! through a full render.

use minijinja::value::Value;
use rust_hedp::filters::{nindent, quote, to_yaml, trim_suffix, trunc};

#[test]
fn nindent_prepends_newline_and_indents_every_line() {
    assert_eq!(nindent("a\nb".into(), 2), "\n  a\n  b");
    assert_eq!(nindent("solo".into(), 4), "\n    solo");
    // An empty string still yields the newline plus padding, matching sprig.
    assert_eq!(nindent(String::new(), 2), "\n  ");
}

#[test]
fn quote_wraps_in_double_quotes() {
    assert_eq!(quote(Value::from("hi")), "\"hi\"");
    assert_eq!(quote(Value::from(7)), "\"7\"");
    assert_eq!(quote(Value::from(())), "\"\"");
}

#[test]
fn trunc_and_trim_suffix_match_sprig() {
    assert_eq!(trunc("abcdef".into(), 3), "abc");
    assert_eq!(trunc("ab".into(), 5), "ab");
    assert_eq!(trim_suffix("name-".into(), "-".into()), "name");
    assert_eq!(trim_suffix("name".into(), "-".into()), "name");
}

#[test]
fn to_yaml_sorts_keys_and_quotes_ambiguous_scalars() {
    let v: serde_json::Value = serde_json::json!({
        "memory": "512Mi",
        "cpu": "1",
        "enabled": true,
        "count": 3,
    });
    let out = to_yaml(Value::from_serialize(&v)).unwrap();
    // Keys sorted, and "1" quoted so it does not read back as a number - the
    // behaviour Helm inherits from sigs.k8s.io/yaml.
    assert_eq!(out, "count: 3\ncpu: \"1\"\nenabled: true\nmemory: 512Mi");
}

#[test]
fn to_yaml_renders_nested_and_empty_collections() {
    let v: serde_json::Value = serde_json::json!({
        "limits": {"cpu": "2"},
        "hosts": [],
        "empty": {},
    });
    let out = to_yaml(Value::from_serialize(&v)).unwrap();
    assert_eq!(out, "empty: {}\nhosts: []\nlimits:\n  cpu: \"2\"");
}
